package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/credential"
)

func main() {
	systray.Run(onReady, onExit)
}

var (
	daemonCmd  *exec.Cmd
	daemonDone chan struct{} // closed by the background goroutine when the process exits
	daemonMu   sync.Mutex

	// startGrace tracks the deadline until which the health poll should
	// not flip the UI to "stopped". This prevents the 3-second poll from
	// racing with daemon startup (which takes several seconds).
	startGrace   time.Time
	startGraceMu sync.Mutex
)

func onReady() {
	systray.SetTitle("M")
	systray.SetTooltip("memoryd – memory layer for coding agents")

	mStatus := systray.AddMenuItem("Status: checking...", "Daemon status")
	mStatus.Disable()

	mMongo := systray.AddMenuItem("MongoDB: checking...", "MongoDB connection status")
	mMongo.Disable()

	systray.AddSeparator()

	// --- Mode submenu ---
	mMode := systray.AddMenuItem("Mode", "How memoryd integrates with your agent")
	mModeProxy := mMode.AddSubMenuItem("Proxy – auto read & write", "Proxy intercepts API calls, captures everything automatically")
	mModeMCP := mMode.AddSubMenuItem("MCP – read & write", "Agent uses MCP tools to search and store explicitly")
	mModeMCPRO := mMode.AddSubMenuItem("MCP – read only", "Agent can search memories but cannot store new ones")

	systray.AddSeparator()

	mToggle := systray.AddMenuItem("Start", "Start or stop the daemon")
	mDash := systray.AddMenuItem("Open Dashboard", "Open web dashboard in browser")
	mConnectMongo := systray.AddMenuItem("Connect to MongoDB...", "Configure MongoDB connection (local Docker or Atlas)")
	mSetAPIKey := systray.AddMenuItem("Set Anthropic API Key...", "Store API key in OS Keychain for LLM synthesis")
	mConfig := systray.AddMenuItem("Open config", "Open config file in editor")
	mLogs := systray.AddMenuItem("Open logs directory", "Open ~/.memoryd in Finder")
	mRegenToken := systray.AddMenuItem("Regenerate API token...", "Create a new dashboard token and restart the daemon")

	// Show initial API key indicator.
	if key, _ := credential.Get("anthropic_api_key"); key != "" {
		mSetAPIKey.SetTitle("Set Anthropic API Key  ✓")
	}

	systray.AddSeparator()

	mUninstall := systray.AddMenuItem("Uninstall memoryd...", "Remove memoryd and all its data")

	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit", "Quit memoryd tray")

	cfg, _ := config.Load()
	port := cfg.Port

	// Apply initial mode checkmarks.
	setModeChecks(cfg.Mode, mModeProxy, mModeMCP, mModeMCPRO)

	binaryPath := findBinary()

	// Auto-start daemon on launch if not already running.
	if !checkHealth(port) {
		setStartGrace()
		startDaemon(binaryPath)
	}

	// Poll daemon health every 3 seconds.
	running := false
	synthActive := false
	mongoConnected := false
	go func() {
		for {
			health := getHealth(port)
			ok := health != nil
			// During startup grace period, don't flip the UI to "stopped".
			if !ok && inStartGrace() {
				time.Sleep(3 * time.Second)
				continue
			}

			// Track synthesis status changes.
			if ok {
				newSynth, _ := health["synthesis"].(bool)
				if newSynth != synthActive {
					synthActive = newSynth
				}
			}

			// Track MongoDB connection status.
			if ok {
				mongoStr, _ := health["mongodb"].(string)
				newMongoConnected := mongoStr == "connected"
				if newMongoConnected != mongoConnected {
					mongoConnected = newMongoConnected
					if mongoConnected {
						mMongo.SetTitle("MongoDB: ✅ connected")
					} else if mongoStr == "connecting" {
						mMongo.SetTitle("MongoDB: 🔄 connecting...")
					} else {
						mMongo.SetTitle("MongoDB: ❌ disconnected — start MongoDB to continue")
					}
				}
			}

			if ok != running {
				running = ok
				if running {
					subtitle := fmt.Sprintf("Status: ● running on port %d", port)
					if synthActive {
						subtitle += "  ✦ synthesis"
					}
					mStatus.SetTitle(subtitle)
					mToggle.SetTitle("Stop")
					if mongoConnected {
						systray.SetTitle("M●")
					} else {
						systray.SetTitle("M⚠")
					}
				} else {
					synthActive = false
					mongoConnected = false
					mStatus.SetTitle("Status: ○ stopped")
					mMongo.SetTitle("MongoDB: —")
					mToggle.SetTitle("Start")
					systray.SetTitle("M○")
				}
			}
			time.Sleep(3 * time.Second)
		}
	}()

	for {
		select {
		case <-mModeProxy.ClickedCh:
			switchMode(config.ModeProxy, mModeProxy, mModeMCP, mModeMCPRO, binaryPath, &running)

		case <-mModeMCP.ClickedCh:
			switchMode(config.ModeMCP, mModeProxy, mModeMCP, mModeMCPRO, binaryPath, &running)

		case <-mModeMCPRO.ClickedCh:
			switchMode(config.ModeMCPReadOnly, mModeProxy, mModeMCP, mModeMCPRO, binaryPath, &running)

		case <-mToggle.ClickedCh:
			if running {
				stopDaemon()
				running = false
				mongoConnected = false
				mToggle.SetTitle("Start")
				mStatus.SetTitle("Status: ○ stopped")
				mMongo.SetTitle("MongoDB: —")
				systray.SetTitle("M○")
			} else {
				setStartGrace()
				startDaemon(binaryPath)
				running = true
				mToggle.SetTitle("Stop")
				mStatus.SetTitle("Status: ● running on port " + fmt.Sprintf("%d", port))
				mMongo.SetTitle("MongoDB: 🔄 connecting...")
				systray.SetTitle("M●")
			}

		case <-mConnectMongo.ClickedCh:
			go connectMongoDialog(binaryPath, &running)

		case <-mSetAPIKey.ClickedCh:
			go setAPIKeyDialog(binaryPath, &running, mSetAPIKey)

		case <-mDash.ClickedCh:
			openDashboard(port)

		case <-mConfig.ClickedCh:
			exec.Command("open", "-t", config.Path()).Start()

		case <-mLogs.ClickedCh:
			exec.Command("open", config.Dir()).Start()

		case <-mRegenToken.ClickedCh:
			go regenTokenDialog(binaryPath, &running)

		case <-mUninstall.ClickedCh:
			go uninstallDialog(binaryPath)

		case <-mQuit.ClickedCh:
			stopDaemon()
			systray.Quit()
		}
	}
}

func onExit() {
	stopDaemon()
}

func checkHealth(port int) bool {
	return getHealth(port) != nil
}

// getHealth fetches the full health response from the daemon, or nil if unreachable.
func getHealth(port int) map[string]any {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		return nil
	}
	if result["status"] != "ok" {
		return nil
	}
	return result
}

// setStartGrace sets a 15-second grace period during which the health poll
// won't flip the UI to "stopped". This covers the time the daemon needs to
// connect to MongoDB, load the embedding model, and start the HTTP server.
func setStartGrace() {
	startGraceMu.Lock()
	startGrace = time.Now().Add(15 * time.Second)
	startGraceMu.Unlock()
}

// inStartGrace returns true if we're within the startup grace period.
func inStartGrace() bool {
	startGraceMu.Lock()
	defer startGraceMu.Unlock()
	return time.Now().Before(startGrace)
}

func startDaemon(binary string) {
	daemonMu.Lock()
	defer daemonMu.Unlock()

	if daemonCmd != nil && daemonCmd.Process != nil {
		return // already running
	}

	cmd := exec.Command(binary, "start")
	// Send logs to a file
	logDir := config.Dir()
	os.MkdirAll(logDir, 0700)
	logPath := filepath.Join(logDir, "daemon.log")
	logFile, err := os.OpenFile(logPath,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		return
	}
	daemonCmd = cmd
	done := make(chan struct{})
	daemonDone = done

	// Wait in background so we can clean up
	go func() {
		err := cmd.Wait()
		daemonMu.Lock()
		if daemonCmd == cmd {
			daemonCmd = nil
			daemonDone = nil
		}
		daemonMu.Unlock()
		close(done) // unblock any stopDaemon waiting on this channel
		if logFile != nil {
			logFile.Close()
		}

		// If the daemon exited with an error, surface the reason to the user.
		if err != nil {
			reason := extractCrashReason(logPath)
			if reason == "" {
				reason = err.Error()
			}
			exec.Command("osascript", "-e",
				fmt.Sprintf(`display notification "%s" with title "memoryd stopped" subtitle "The daemon exited unexpectedly"`, reason)).Run()
		}
	}()
}

func stopDaemon() {
	daemonMu.Lock()
	if daemonCmd == nil || daemonCmd.Process == nil {
		daemonMu.Unlock()
		return
	}
	daemonCmd.Process.Signal(os.Interrupt)
	done := daemonDone // reuse the channel already waited on by startDaemon's goroutine
	daemonMu.Unlock()

	// Wait for the existing background goroutine (which calls cmd.Wait) to confirm exit.
	// Do NOT call Process.Wait() again — concurrent waitpid on the same PID is undefined.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		daemonMu.Lock()
		if daemonCmd != nil && daemonCmd.Process != nil {
			daemonCmd.Process.Kill()
		}
		daemonMu.Unlock()
		// Give Kill a moment to land, then move on.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	daemonMu.Lock()
	daemonCmd = nil
	daemonDone = nil
	daemonMu.Unlock()
}

// extractCrashReason reads the daemon log and returns the last error line.
func extractCrashReason(logPath string) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Walk backwards looking for a line that looks like an error.
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-20; i-- {
		line := lines[i]
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "failed") {
			// Strip the log timestamp prefix if present (e.g., "2024/01/01 12:00:00 ")
			if idx := strings.Index(line, " "); idx > 0 {
				if idx2 := strings.Index(line[idx+1:], " "); idx2 > 0 {
					candidate := line[idx+1+idx2+1:]
					if len(candidate) > 10 {
						line = candidate
					}
				}
			}
			// Truncate for notification display.
			if len(line) > 120 {
				line = line[:120] + "..."
			}
			return line
		}
	}
	return ""
}

func findBinary() string {
	// 1. Same directory as the tray binary
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	candidate := filepath.Join(dir, "memoryd")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// 2. PATH lookup
	if p, err := exec.LookPath("memoryd"); err == nil {
		return p
	}

	// 3. Common install location
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/opt/homebrew/bin/memoryd", "/usr/local/bin/memoryd"} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	return "memoryd" // hope PATH has it
}

func connectMongoDialog(binaryPath string, running *bool) {
	choiceOut, err := exec.Command("osascript",
		"-e", `display dialog "Connect memoryd to MongoDB:\n\n• Local — run MongoDB in Docker on this machine\n• Atlas — connect to a MongoDB Atlas cluster" buttons {"Cancel", "Atlas", "Local (Docker)"} default button "Local (Docker)" with title "memoryd – Connect to MongoDB"`,
		"-e", `button returned of result`,
	).Output()
	if err != nil {
		return // cancelled
	}
	switch strings.TrimSpace(string(choiceOut)) {
	case "Local (Docker)":
		setupLocalMongo(binaryPath, running)
	case "Atlas":
		setupAtlasMongo(binaryPath, running)
	}
}

func setupLocalMongo(binaryPath string, running *bool) {
	if exec.Command("docker", "info").Run() != nil {
		exec.Command("osascript", "-e",
			`display dialog "Docker is not running.\n\nStart Docker Desktop and try again, or use Atlas instead." buttons {"OK"} with icon stop with title "memoryd"`).Run()
		return
	}

	notify := func(msg string) {
		exec.Command("osascript", "-e",
			fmt.Sprintf(`display notification "%s" with title "memoryd"`, msg)).Run()
	}

	// Create or start the container.
	if containerExists("memoryd-mongo") {
		// Check if it's already running.
		out, _ := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
		alreadyRunning := false
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == "memoryd-mongo" {
				alreadyRunning = true
				break
			}
		}
		if !alreadyRunning {
			if err := exec.Command("docker", "start", "memoryd-mongo").Run(); err != nil {
				exec.Command("osascript", "-e",
					fmt.Sprintf(`display dialog "Failed to start MongoDB container: %s" buttons {"OK"} with icon stop with title "memoryd"`, err.Error())).Run()
				return
			}
		}
	} else {
		notify("Creating MongoDB container...")
		if err := exec.Command("docker", "run", "-d", "--name", "memoryd-mongo", "-p", "27017:27017", "mongodb/mongodb-atlas-local:8.0").Run(); err != nil {
			exec.Command("osascript", "-e",
				fmt.Sprintf(`display dialog "Failed to create MongoDB container: %s" buttons {"OK"} with icon stop with title "memoryd"`, err.Error())).Run()
			return
		}
	}

	// Wait for MongoDB to be ready.
	notify("Waiting for MongoDB to be ready...")
	ready := false
	for i := 0; i < 30; i++ {
		out, _ := exec.Command("docker", "exec", "memoryd-mongo", "mongosh", "--quiet", "--eval", "db.runCommand({ping:1}).ok").Output()
		if strings.TrimSpace(string(out)) == "1" {
			ready = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !ready {
		exec.Command("osascript", "-e",
			`display dialog "MongoDB did not become ready in time.\n\nCheck Docker Desktop and try again." buttons {"OK"} with icon stop with title "memoryd"`).Run()
		return
	}

	// Create vector search index (best-effort — Atlas Local may already have it).
	exec.Command("docker", "exec", "memoryd-mongo", "mongosh", "memoryd", "--quiet", "--eval",
		`db.memories.createSearchIndex("vector_index","vectorSearch",{fields:[{type:"vector",numDimensions:1024,path:"embedding",similarity:"cosine"}]})`).Run()

	finishMongoSetup("mongodb://localhost:27017/?directConnection=true", binaryPath, running)
}

func setupAtlasMongo(binaryPath string, running *bool) {
	uriOut, err := exec.Command("osascript",
		"-e", `display dialog "Enter your MongoDB Atlas connection string:" default answer "mongodb+srv://..." buttons {"Cancel", "Connect"} default button "Connect" with title "memoryd – Atlas"`,
		"-e", `text returned of result`,
	).Output()
	if err != nil {
		return // cancelled
	}
	uri := strings.TrimSpace(string(uriOut))
	if uri == "" || uri == "mongodb+srv://..." {
		return
	}
	finishMongoSetup(uri, binaryPath, running)
}

func finishMongoSetup(uri, binaryPath string, running *bool) {
	if err := config.StoreCredential("mongodb_atlas_uri", uri); err != nil {
		exec.Command("osascript", "-e",
			fmt.Sprintf(`display dialog "Failed to save credentials: %s" buttons {"OK"} with icon stop with title "memoryd"`, err.Error())).Run()
		return
	}

	stopDaemon()
	time.Sleep(500 * time.Millisecond)
	setStartGrace()
	startDaemon(binaryPath)
	*running = true

	exec.Command("osascript", "-e",
		`display notification "MongoDB connected — memoryd is starting" with title "memoryd"`).Run()
}

// setModeChecks updates submenu check marks to reflect the active mode.
func setModeChecks(mode string, proxy, mcp, mcpRO *systray.MenuItem) {
	proxy.Uncheck()
	mcp.Uncheck()
	mcpRO.Uncheck()

	switch mode {
	case config.ModeMCP:
		mcp.Check()
	case config.ModeMCPReadOnly:
		mcpRO.Check()
	default: // proxy is the default
		proxy.Check()
	}
}

// switchMode persists the new mode, updates submenu checks, and restarts the daemon.
func switchMode(mode string, proxy, mcp, mcpRO *systray.MenuItem, binaryPath string, running *bool) {
	if err := config.SetMode(mode); err != nil {
		exec.Command("osascript", "-e",
			fmt.Sprintf(`display dialog "Failed to set mode: %s" buttons {"OK"} with icon stop with title "memoryd"`, err.Error())).Run()
		return
	}

	setModeChecks(mode, proxy, mcp, mcpRO)

	// Restart daemon so it picks up the new mode.
	if *running {
		stopDaemon()
		time.Sleep(500 * time.Millisecond)
		setStartGrace()
		startDaemon(binaryPath)
	}

	label := map[string]string{
		config.ModeProxy:       "Proxy – auto read & write",
		config.ModeMCP:         "MCP – read & write",
		config.ModeMCPReadOnly: "MCP – read only",
	}[mode]
	exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "Mode set to: %s" with title "memoryd"`, label)).Run()
}

// regenTokenDialog confirms, deletes the token file, and restarts the daemon.
func regenTokenDialog(binaryPath string, running *bool) {
	_, err := exec.Command("osascript",
		"-e", `display dialog "This will generate a new API token and restart the daemon.\n\nAny browser sessions will need to be re-opened from the tray." buttons {"Cancel", "Regenerate"} default button "Cancel" with title "memoryd – Regenerate Token"`,
		"-e", `button returned of result`).Output()
	if err != nil {
		return // cancelled
	}

	if err := os.Remove(config.TokenPath()); err != nil && !os.IsNotExist(err) {
		exec.Command("osascript", "-e",
			fmt.Sprintf(`display dialog "Could not remove token file: %s" buttons {"OK"} with icon stop with title "memoryd"`, err.Error())).Run()
		return
	}

	stopDaemon()
	time.Sleep(500 * time.Millisecond)
	setStartGrace()
	startDaemon(binaryPath)
	*running = true

	exec.Command("osascript", "-e",
		`display notification "New token generated — use Open Dashboard to access the UI" with title "memoryd"`).Run()
}

// setAPIKeyDialog prompts for the Anthropic API key, stores it in the OS
// Keychain, and restarts the daemon so it picks up the new key.
func setAPIKeyDialog(binaryPath string, running *bool, menuItem *systray.MenuItem) {
	// Check if a key is already set to offer a Remove option.
	existing, _ := credential.Get("anthropic_api_key")

	var buttons string
	var msg string
	if existing != "" {
		buttons = `buttons {"Cancel", "Remove", "Save"} default button "Save"`
		msg = fmt.Sprintf("Current key: %s\\n\\nPaste a new key to replace it, or click Remove.", maskKey(existing))
	} else {
		buttons = `buttons {"Cancel", "Save"} default button "Save"`
		msg = "Paste your Anthropic API key (sk-ant-...).\\n\\nStored securely in the macOS Keychain."
	}

	// Use two separate osascript calls: one for the dialog, then extract
	// text and button separately to avoid comma-in-key parsing issues.
	script := fmt.Sprintf(`set dr to display dialog "%s" default answer "" %s with hidden answer with title "memoryd – Anthropic API Key"
set btn to button returned of dr
set val to text returned of dr
return val & "\n" & btn`, msg, buttons)

	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return // cancelled
	}

	// Split on newline — value is everything before the last line, button is the last line.
	raw := strings.TrimSpace(string(out))
	idx := strings.LastIndex(raw, "\n")
	if idx < 0 {
		return
	}
	value := raw[:idx]
	button := strings.TrimSpace(raw[idx+1:])

	switch button {
	case "Remove":
		credential.Delete("anthropic_api_key")
		menuItem.SetTitle("Set Anthropic API Key...")
		exec.Command("osascript", "-e",
			`display notification "API key removed from Keychain" with title "memoryd"`).Run()
	case "Save":
		if value == "" {
			return // nothing entered
		}
		if err := credential.Set("anthropic_api_key", value); err != nil {
			exec.Command("osascript", "-e",
				fmt.Sprintf(`display dialog "Failed to save API key: %s" buttons {"OK"} with icon stop with title "memoryd"`, err.Error())).Run()
			return
		}
		menuItem.SetTitle("Set Anthropic API Key  ✓")
		exec.Command("osascript", "-e",
			`display notification "API key saved to Keychain" with title "memoryd"`).Run()
	default:
		return
	}

	// Restart daemon to pick up the change.
	if *running {
		stopDaemon()
		time.Sleep(500 * time.Millisecond)
		setStartGrace()
		startDaemon(binaryPath)
	}
}

// maskKey returns a masked version of an API key for display, or empty string.
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 12 {
		return "••••••••"
	}
	return key[:7] + "••••" + key[len(key)-4:]
}

// uninstallDialog confirms with the user and removes memoryd components.
func uninstallDialog(binaryPath string) {
	// Confirm.
	_, err := exec.Command("osascript", "-e",
		`display dialog "This will stop the daemon and remove:\n\n• memoryd binary & llama-server\n• Memoryd.app\n• ~/.memoryd (config, models, logs)\n• Docker container (memoryd-mongo)\n• MCP config entries (Claude Code, Claude Desktop, Cursor, Windsurf)\n• Stored credentials (OS keychain)\n\nThis cannot be undone." buttons {"Cancel", "Uninstall"} default button "Cancel" with icon caution with title "Uninstall memoryd"`, "-e",
		`button returned of result`).Output()
	if err != nil {
		return // user cancelled
	}

	// 1. Stop daemon.
	stopDaemon()

	// 2. Kill llama-server (embedding subprocess).
	exec.Command("pkill", "-f", "llama-server").Run()

	var removed []string
	var failed []string

	// 2b. Remove keychain credentials.
	config.DeleteCredentials()
	removed = append(removed, "Keychain credentials")

	// 3. Remove MCP config entries from all known agents.
	home, _ := os.UserHomeDir()
	agentConfigs := []struct {
		name string
		path string
	}{
		{"Claude Code", filepath.Join(home, ".mcp.json")},
		{"Claude Desktop", filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")},
		{"Cursor", filepath.Join(home, ".cursor", "mcp.json")},
		{"Windsurf", filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")},
	}
	for _, ac := range agentConfigs {
		if cleanMCPConfig(ac.path) {
			removed = append(removed, ac.name+" MCP config")
		}
	}

	// 5. Stop and remove Docker container.
	if containerExists("memoryd-mongo") {
		exec.Command("docker", "stop", "memoryd-mongo").Run()
		if exec.Command("docker", "rm", "memoryd-mongo").Run() == nil {
			removed = append(removed, "Docker container")
		} else {
			failed = append(failed, "Docker container (remove manually: docker rm memoryd-mongo)")
		}
	}

	// 6. Remove ~/.memoryd.
	memorydDir := config.Dir()
	if _, err := os.Stat(memorydDir); err == nil {
		if os.RemoveAll(memorydDir) == nil {
			removed = append(removed, "~/.memoryd")
		} else {
			failed = append(failed, "~/.memoryd (permission denied)")
		}
	}

	// 7. Remove Memoryd.app.
	appPath := "/Applications/Memoryd.app"
	if _, err := os.Stat(appPath); err == nil {
		if os.RemoveAll(appPath) == nil {
			removed = append(removed, "Memoryd.app")
		} else {
			failed = append(failed, "Memoryd.app (remove manually)")
		}
	}

	// 8. Remove binaries.
	if binaryPath != "" && binaryPath != "memoryd" {
		if os.Remove(binaryPath) == nil {
			removed = append(removed, "memoryd binary")
		} else {
			if exec.Command("sudo", "rm", "-f", binaryPath).Run() == nil {
				removed = append(removed, "memoryd binary")
			} else {
				failed = append(failed, fmt.Sprintf("binary at %s (remove manually)", binaryPath))
			}
		}
	}

	// Remove llama-server if we installed it.
	llamaPath := filepath.Join(filepath.Dir(binaryPath), "llama-server")
	if binaryPath != "" && binaryPath != "memoryd" {
		if _, err := os.Stat(llamaPath); err == nil {
			if os.Remove(llamaPath) == nil {
				removed = append(removed, "llama-server binary")
			} else {
				exec.Command("sudo", "rm", "-f", llamaPath).Run()
				removed = append(removed, "llama-server binary")
			}
		}
	}

	// 9. Show result.
	msg := "memoryd has been uninstalled."
	if len(removed) > 0 {
		msg += "\n\nRemoved:\n• " + strings.Join(removed, "\n• ")
	}
	if len(failed) > 0 {
		msg += "\n\nCould not remove:\n• " + strings.Join(failed, "\n• ")
	}
	msg += "\n\nThe tray app will now quit."

	exec.Command("osascript", "-e",
		fmt.Sprintf(`display dialog "%s" buttons {"OK"} with title "memoryd"`, msg)).Run()

	systray.Quit()
}

// cleanMCPConfig removes the memoryd entry from an MCP config file.
// Works with any agent that uses the standard {"mcpServers": {...}} format.
func cleanMCPConfig(configPath string) bool {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}

	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}

	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return false
	}

	if _, exists := servers["memoryd"]; !exists {
		return false
	}

	delete(servers, "memoryd")
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false
	}

	return os.WriteFile(configPath, out, 0600) == nil
}

func openDashboard(port int) {
	token := config.LoadToken()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if token != "" {
		url += "?token=" + token
	}
	exec.Command("open", url).Start()
}

// containerExists checks if a Docker container exists (running or stopped).
func containerExists(name string) bool {
	out, err := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}
