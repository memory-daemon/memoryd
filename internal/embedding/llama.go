package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const embeddingServerPort = "7433"
const maxBatchSize = 4 // max texts per EmbedBatch call to avoid KV cache overflow

// LlamaEmbedder runs a local llama.cpp server subprocess and embeds via HTTP.
type LlamaEmbedder struct {
	cmd       *exec.Cmd
	serverURL string
	dim       int
	client    *http.Client
	closeOnce sync.Once
}

// NewLlamaEmbedder starts llama-server with the given GGUF model.
// Requires llama.cpp to be installed (brew install llama.cpp).
func NewLlamaEmbedder(modelPath string, dim int) (*LlamaEmbedder, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("model not found at %s — download the GGUF from HuggingFace", modelPath)
	}

	serverURL := "http://127.0.0.1:" + embeddingServerPort

	logPath := filepath.Join(os.TempDir(), "memoryd-llama-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("creating llama-server log: %w", err)
	}

	cmd := exec.Command("llama-server",
		"-m", modelPath,
		"--port", embeddingServerPort,
		"--embedding",
		"-c", "2048",
		"-np", "1",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Put llama-server in its own process group so we can kill the group
	// and ensure no children outlive the daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting llama-server: %w (is llama.cpp installed? brew install llama.cpp)", err)
	}

	e := &LlamaEmbedder{
		cmd:       cmd,
		serverURL: serverURL,
		dim:       dim,
		client:    &http.Client{Timeout: 30 * time.Second},
	}

	if err := e.waitReady(); err != nil {
		cmd.Process.Kill()
		return nil, err
	}

	return e, nil
}

func (e *LlamaEmbedder) waitReady() error {
	for i := 0; i < 30; i++ {
		resp, err := e.client.Get(e.serverURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("llama-server did not become ready within 15s (check %s/memoryd-llama-server.log)", os.TempDir())
}

type embeddingReq struct {
	Input string `json:"input"`
}

type embeddingBatchReq struct {
	Input []string `json:"input"`
}

type embeddingResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *LlamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embeddingReq{Input: text})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.serverURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding server returned %d", resp.StatusCode)
	}

	var result embeddingResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}

	return result.Data[0].Embedding, nil
}

func (e *LlamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) == 1 {
		vec, err := e.Embed(ctx, texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float32{vec}, nil
	}

	// Process in small batches to avoid overflowing the KV cache.
	if len(texts) > maxBatchSize {
		all := make([][]float32, 0, len(texts))
		for i := 0; i < len(texts); i += maxBatchSize {
			end := i + maxBatchSize
			if end > len(texts) {
				end = len(texts)
			}
			vecs, err := e.EmbedBatch(ctx, texts[i:end])
			if err != nil {
				return nil, err
			}
			all = append(all, vecs...)
		}
		return all, nil
	}

	body, err := json.Marshal(embeddingBatchReq{Input: texts})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.serverURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding server returned %d", resp.StatusCode)
	}

	var result embeddingResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding batch embedding response: %w", err)
	}

	if len(result.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(result.Data))
	}

	vecs := make([][]float32, len(texts))
	for i, d := range result.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

func (e *LlamaEmbedder) Dim() int { return e.dim }

func (e *LlamaEmbedder) Close() error {
	var err error
	e.closeOnce.Do(func() {
		if e.cmd == nil || e.cmd.Process == nil {
			return
		}
		// Kill the entire process group (llama-server + any children).
		pgid, pgErr := syscall.Getpgid(e.cmd.Process.Pid)
		if pgErr == nil {
			// Negative PID = signal the process group.
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			e.cmd.Process.Signal(syscall.SIGTERM)
		}

		// Give it a moment to exit cleanly, then force-kill.
		done := make(chan struct{})
		go func() {
			e.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if pgErr == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				e.cmd.Process.Kill()
			}
			<-done // reap
		}
	})
	return err
}
