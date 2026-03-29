// benchmark.go
//
// Comprehensive HuggingFace dataset benchmark for memoryd.
//
// Feeds N rows from nlile/misc-merged-claude-code-traces-v1 through the live
// /api/ingest endpoint and records detailed per-row results. Produces:
//
//  1. A JSONL file with every row's classification, pipeline stage, latency,
//     input/output lengths, and synthesis entry text.
//  2. A markdown report with cross-tabulations, distributions, and search
//     quality probes — designed for "analyzing every which way."
//
// Usage:
//
//	go run ./cmd/corpus-eval --benchmark [--sample 1000] [--concurrency 4]
//	go run ./cmd/corpus-eval --benchmark --sample 5000 --output report.md
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Per-row result — the full record for every ingested row
// ---------------------------------------------------------------------------

type benchRow struct {
	Index             int    `json:"index"`
	Classification    string `json:"classification"` // noise | explanation | substantive
	Stage             string `json:"stage"`          // pre_filter | synthesizer_skip | stored | noise_filtered | error
	Stored            int    `json:"stored"`         // 0 or 1+
	LatencyMs         int64  `json:"latency_ms"`
	UserPromptLen     int    `json:"user_prompt_len"`
	AssistantLen      int    `json:"assistant_len"`
	EntryLen          int    `json:"entry_len"` // length of synthesized entry (0 if not stored)
	Summary           string `json:"summary"`   // WriteResult.Summary()
	WritePipeStored   int    `json:"wp_stored"`
	WritePipeDupes    int    `json:"wp_dupes"`
	WritePipeFiltered int    `json:"wp_filtered"`
	WritePipeExtended int    `json:"wp_extended"`
	WritePipeMerged   int    `json:"wp_merged"`
	Error             string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Search quality probe
// ---------------------------------------------------------------------------

var benchSearchQueries = []string{
	"how does deduplication work in the memory pipeline",
	"debugging memory retrieval when search returns empty results",
	"MongoDB vector search configuration and indexing",
	"security redaction and sensitive data handling",
	"write pipeline performance and latency bottlenecks",
	"how to configure memoryd for a team",
	"error handling best practices in Go",
	"API authentication and token management",
	"embedding model and vector dimensions",
	"steward quality scoring and pruning",
}

type searchProbe struct {
	Query    string  `json:"query"`
	TopScore float64 `json:"top_score"`
	NumHits  int     `json:"num_hits"`
	Preview  string  `json:"preview"`
}

// ---------------------------------------------------------------------------
// Main benchmark entry point
// ---------------------------------------------------------------------------

func runBenchmark(w io.Writer, baseURL string, sampleSize, concurrency int, noCleanup bool, jsonlPath, dataDir string) {
	token := loadToken()

	fmt.Fprintf(os.Stderr, "=== memoryd comprehensive benchmark ===\n")
	fmt.Fprintf(os.Stderr, "dataset:     %s\n", hfDataset)
	fmt.Fprintf(os.Stderr, "sample:      %d rows\n", sampleSize)
	fmt.Fprintf(os.Stderr, "concurrency: %d\n", concurrency)
	fmt.Fprintf(os.Stderr, "target:      %s\n", baseURL)
	if dataDir != "" {
		fmt.Fprintf(os.Stderr, "data-dir:    %s\n", dataDir)
	}
	if jsonlPath != "" {
		fmt.Fprintf(os.Stderr, "jsonl:       %s\n", jsonlPath)
	}
	fmt.Fprintln(os.Stderr)

	// ---- 1. Health check ------------------------------------------------
	var health struct{ Status string }
	if err := authGet(baseURL, "/health", &health, ""); err != nil || health.Status != "ok" {
		fmt.Fprintf(os.Stderr, "error: memoryd not reachable at %s\n", baseURL)
		os.Exit(1)
	}

	// ---- 2. Load rows ---------------------------------------------------
	var rows []hfRow
	var err error
	if dataDir != "" {
		fmt.Fprintln(os.Stderr, "[1/5] loading rows from local JSONL...")
		rows, err = loadLocalJSONL(dataDir, sampleSize)
	} else {
		fmt.Fprintln(os.Stderr, "[1/5] fetching rows from HuggingFace API...")
		rows, err = fetchHFRows(sampleSize)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "      loaded %d rows\n\n", len(rows))

	// ---- 3. Classify locally --------------------------------------------
	fmt.Fprintln(os.Stderr, "[2/5] classifying rows locally...")
	classifications := make([]respClass, len(rows))
	classCounts := map[string]int{}
	for i, row := range rows {
		classifications[i] = classifyResponse(row.AssistantResponse)
		classCounts[classifications[i].String()]++
	}
	fmt.Fprintf(os.Stderr, "      noise=%d  explanation=%d  substantive=%d\n\n",
		classCounts["noise"], classCounts["explanation"], classCounts["substantive"])

	// ---- 4. Pre-ingest snapshot -----------------------------------------
	fmt.Fprintln(os.Stderr, "[3/5] pre-ingest snapshot...")
	var preDash dashResp
	_ = authGet(baseURL, "/api/dashboard", &preDash, token)
	fmt.Fprintf(os.Stderr, "      memories before: %d\n\n", preDash.MemoryCount)

	// ---- 5. Ingest with worker pool -------------------------------------
	fmt.Fprintf(os.Stderr, "[4/5] ingesting %d rows through /api/ingest (%d workers)...\n", len(rows), concurrency)
	evalSource := fmt.Sprintf("benchmark-%d", time.Now().UnixNano())

	benchResults := make([]benchRow, len(rows))
	var progress atomic.Int64
	wallStart := time.Now()

	type work struct {
		row   hfRow
		idx   int
		class respClass
	}
	workCh := make(chan work, concurrency*2)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ingestClient := &http.Client{Timeout: 120 * time.Second}
			for job := range workCh {
				br := benchRow{
					Index:          job.idx,
					Classification: job.class.String(),
					UserPromptLen:  len(job.row.UserPrompt),
					AssistantLen:   len(strings.TrimSpace(job.row.AssistantResponse)),
				}

				ar := strings.TrimSpace(job.row.AssistantResponse)
				if len(ar) > 32*1024 {
					ar = ar[:32*1024]
				}

				body, _ := json.Marshal(map[string]string{
					"user_prompt":        job.row.UserPrompt,
					"assistant_response": ar,
					"source":             evalSource,
				})

				t0 := time.Now()
				req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/ingest", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := ingestClient.Do(req)
				br.LatencyMs = time.Since(t0).Milliseconds()

				if err != nil {
					br.Stage = "error"
					br.Error = err.Error()
					benchResults[job.idx] = br
					progress.Add(1)
					continue
				}

				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				if resp.StatusCode >= 400 {
					br.Stage = "error"
					br.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
					benchResults[job.idx] = br
					progress.Add(1)
					continue
				}

				var ir ingestResp
				_ = json.Unmarshal(data, &ir)

				br.Stage = ir.Stage
				br.Stored = ir.Stored
				br.EntryLen = len(ir.Entry)
				br.Summary = ir.Summary

				// Parse write pipeline summary for detail.
				s, ext, d, m, f := parseSummary(ir.Summary)
				br.WritePipeStored = s
				br.WritePipeDupes = d
				br.WritePipeFiltered = f
				br.WritePipeExtended = ext
				br.WritePipeMerged = m

				benchResults[job.idx] = br
				n := progress.Add(1)
				if n%200 == 0 {
					elapsed := time.Since(wallStart)
					rate := float64(n) / elapsed.Seconds()
					eta := time.Duration(float64(len(rows)-int(n))/rate) * time.Second
					fmt.Fprintf(os.Stderr, "      %d / %d (%.0f rows/s, ETA %s)\n", n, len(rows), rate, eta.Round(time.Second))
				}
			}
		}()
	}

	for i, row := range rows {
		workCh <- work{row: row, idx: i, class: classifications[i]}
	}
	close(workCh)
	wg.Wait()

	wallElapsed := time.Since(wallStart)
	fmt.Fprintf(os.Stderr, "      done in %s (%.1f rows/sec)\n\n", wallElapsed.Round(time.Second), float64(len(rows))/wallElapsed.Seconds())

	// ---- 6. Post-ingest snapshot ----------------------------------------
	fmt.Fprintln(os.Stderr, "[5/5] post-ingest snapshot + search probes...")
	var postDash dashResp
	_ = authGet(baseURL, "/api/dashboard", &postDash, token)
	fmt.Fprintf(os.Stderr, "      memories after: %d (delta: +%d)\n", postDash.MemoryCount, postDash.MemoryCount-preDash.MemoryCount)

	// Search quality probes.
	var probes []searchProbe
	for _, q := range benchSearchQueries {
		var sr searchResp
		if err := authPost(baseURL, "/api/search", searchReq{Query: q}, &sr, token); err != nil {
			probes = append(probes, searchProbe{Query: q})
			continue
		}
		sp := searchProbe{
			Query:   q,
			NumHits: len(sr.Scores),
			Preview: truncate(extractFirstChunk(sr.Context, 120), 120),
		}
		if len(sr.Scores) > 0 {
			sp.TopScore = sr.Scores[0]
		}
		probes = append(probes, sp)
	}

	// Fetch rejection stats.
	var rejResp struct {
		Stats struct {
			Total    int            `json:"total"`
			Capacity int            `json:"capacity"`
			ByStage  map[string]int `json:"by_stage"`
		} `json:"stats"`
	}
	_ = authGet(baseURL, "/api/rejections?n=0", &rejResp, token)

	// ---- 7. Write JSONL -------------------------------------------------
	if jsonlPath != "" {
		fmt.Fprintf(os.Stderr, "      writing JSONL to %s...\n", jsonlPath)
		if err := writeJSONL(jsonlPath, benchResults); err != nil {
			fmt.Fprintf(os.Stderr, "      error writing JSONL: %v\n", err)
		}
	}

	// ---- 8. Cleanup -----------------------------------------------------
	cleanupCount := -1
	if !noCleanup {
		fmt.Fprintln(os.Stderr, "      cleaning up benchmark memories...")
		cleanupCount = deleteBySource(baseURL, token, evalSource)
		fmt.Fprintf(os.Stderr, "      deleted %d memories\n", cleanupCount)
	}

	// ---- 9. Write report ------------------------------------------------
	writeBenchReport(w, benchResults, classifications, rows, probes,
		preDash.MemoryCount, postDash.MemoryCount, wallElapsed,
		rejResp.Stats, cleanupCount, evalSource)
}

// ---------------------------------------------------------------------------
// JSONL output
// ---------------------------------------------------------------------------

func writeJSONL(path string, results []benchRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Report generation
// ---------------------------------------------------------------------------

func writeBenchReport(w io.Writer, results []benchRow, classes []respClass, rows []hfRow,
	probes []searchProbe,
	memsBefore, memsAfter int, wallTime time.Duration,
	rejStats struct {
		Total    int            `json:"total"`
		Capacity int            `json:"capacity"`
		ByStage  map[string]int `json:"by_stage"`
	},
	cleanupCount int, source string,
) {
	total := len(results)
	now := time.Now().Format("2006-01-02 15:04:05")

	fmt.Fprintf(w, "# memoryd Comprehensive Benchmark — %s\n\n", now)
	fmt.Fprintf(w, "**Dataset:** `%s`  \n", hfDataset)
	fmt.Fprintf(w, "**Rows processed:** %d  \n", total)
	fmt.Fprintf(w, "**Wall time:** %s  \n", wallTime.Round(time.Second))
	if wallTime.Seconds() > 0 {
		fmt.Fprintf(w, "**Throughput:** %.1f rows/sec  \n", float64(total)/wallTime.Seconds())
	}
	fmt.Fprintf(w, "**Memories before:** %d  \n", memsBefore)
	fmt.Fprintf(w, "**Memories after:** %d (delta: +%d)  \n", memsAfter, memsAfter-memsBefore)
	fmt.Fprintf(w, "**Source tag:** `%s`  \n\n", source)

	// ================================================================
	// Section 1: Classification Distribution
	// ================================================================
	fmt.Fprintf(w, "---\n\n## 1. Local Classification Distribution\n\n")
	fmt.Fprintf(w, "Each row classified locally before ingestion (same heuristics as the HF pipeline optimiser).\n\n")

	classCount := map[string]int{}
	for _, c := range classes {
		classCount[c.String()]++
	}
	fmt.Fprintln(w, "| Class | Count | % |")
	fmt.Fprintln(w, "|-------|-------|---|")
	for _, cls := range []string{"noise", "explanation", "substantive"} {
		n := classCount[cls]
		fmt.Fprintf(w, "| %s | %d | %.1f%% |\n", cls, n, pct(n, total))
	}
	fmt.Fprintln(w)

	// ================================================================
	// Section 2: Pipeline Stage Distribution
	// ================================================================
	fmt.Fprintf(w, "## 2. Pipeline Stage Distribution\n\n")

	stageCount := map[string]int{}
	for _, r := range results {
		stageCount[r.Stage]++
	}
	stageOrder := []string{"stored", "synthesizer_skip", "pre_filter", "noise_filtered", "no_synthesizer", "error"}
	stageDesc := map[string]string{
		"stored":           "Distilled entry reached MongoDB",
		"synthesizer_skip": "LLM judged exchange as no durable value",
		"pre_filter":       "Cheap heuristic (ack + procedural prefix)",
		"noise_filtered":   "Write pipeline noise gate (no synthesizer path)",
		"no_synthesizer":   "Synthesizer unavailable, raw write",
		"error":            "Network / API error",
	}

	fmt.Fprintln(w, "| Stage | Count | % | Description |")
	fmt.Fprintln(w, "|-------|-------|---|-------------|")
	totalFiltered := 0
	for _, st := range stageOrder {
		n := stageCount[st]
		if n == 0 {
			continue
		}
		if st == "pre_filter" || st == "synthesizer_skip" || st == "noise_filtered" {
			totalFiltered += n
		}
		fmt.Fprintf(w, "| %s | %d | %.1f%% | %s |\n", st, n, pct(n, total), stageDesc[st])
	}
	fmt.Fprintln(w)

	stored := stageCount["stored"]
	fmt.Fprintf(w, "**Store rate:** %d / %d (%.1f%%)  \n", stored, total, pct(stored, total))
	fmt.Fprintf(w, "**Total filtered:** %d / %d (%.1f%%)  \n\n", totalFiltered, total, pct(totalFiltered, total))

	// ================================================================
	// Section 3: Cross-tabulation — Classification × Stage
	// ================================================================
	fmt.Fprintf(w, "## 3. Cross-Tabulation: Classification × Pipeline Stage\n\n")
	fmt.Fprintf(w, "How did each classification category flow through the pipeline?\n\n")

	// Build the cross-tab.
	crossTab := map[string]map[string]int{}
	classOrder := []string{"noise", "explanation", "substantive"}
	for _, cls := range classOrder {
		crossTab[cls] = map[string]int{}
	}
	for _, r := range results {
		crossTab[r.Classification][r.Stage]++
	}

	// Active stages (those with any count).
	var activeStages []string
	for _, st := range stageOrder {
		if stageCount[st] > 0 {
			activeStages = append(activeStages, st)
		}
	}

	// Header.
	fmt.Fprintf(w, "| Classification |")
	for _, st := range activeStages {
		fmt.Fprintf(w, " %s |", st)
	}
	fmt.Fprintf(w, " Total |\n|---|")
	for range activeStages {
		fmt.Fprintf(w, "---|")
	}
	fmt.Fprintf(w, "---|\n")

	for _, cls := range classOrder {
		row := crossTab[cls]
		total := classCount[cls]
		fmt.Fprintf(w, "| **%s** |", cls)
		for _, st := range activeStages {
			n := row[st]
			if total > 0 {
				fmt.Fprintf(w, " %d (%.0f%%) |", n, float64(n)/float64(total)*100)
			} else {
				fmt.Fprintf(w, " 0 |")
			}
		}
		fmt.Fprintf(w, " %d |\n", total)
	}
	fmt.Fprintln(w)

	// Key insight.
	if classCount["explanation"] > 0 {
		explStored := crossTab["explanation"]["stored"]
		explTotal := classCount["explanation"]
		fmt.Fprintf(w, "**Key metric — explanation leak rate:** %d / %d explanation rows stored (%.1f%%). ",
			explStored, explTotal, pct(explStored, explTotal))
		fmt.Fprintf(w, "Lower is better — these carry no durable knowledge.\n\n")
	}
	if classCount["substantive"] > 0 {
		substFiltered := classCount["substantive"] - crossTab["substantive"]["stored"]
		substTotal := classCount["substantive"]
		fmt.Fprintf(w, "**Key metric — substantive false-positive rate:** %d / %d substantive rows filtered (%.1f%%). ",
			substFiltered, substTotal, pct(substFiltered, substTotal))
		fmt.Fprintf(w, "Lower is better — these should be stored.\n\n")
	}

	// ================================================================
	// Section 4: Content Length Analysis
	// ================================================================
	fmt.Fprintf(w, "## 4. Content Length Analysis\n\n")

	// Length buckets.
	type bucket struct {
		label string
		lo    int
		hi    int
	}
	buckets := []bucket{
		{"< 100", 0, 100},
		{"100–500", 100, 500},
		{"500–1K", 500, 1000},
		{"1K–5K", 1000, 5000},
		{"5K–10K", 5000, 10000},
		{"10K–32K", 10000, 32001},
	}

	fmt.Fprintf(w, "### Assistant response length × pipeline outcome\n\n")
	fmt.Fprintf(w, "| Length bucket | Total | Stored | Filtered | Store rate |\n")
	fmt.Fprintf(w, "|-------------|-------|--------|----------|------------|\n")
	for _, b := range buckets {
		var bTotal, bStored int
		for _, r := range results {
			if r.AssistantLen >= b.lo && r.AssistantLen < b.hi {
				bTotal++
				if r.Stage == "stored" {
					bStored++
				}
			}
		}
		if bTotal == 0 {
			continue
		}
		fmt.Fprintf(w, "| %s chars | %d | %d | %d | %.0f%% |\n",
			b.label, bTotal, bStored, bTotal-bStored, pct(bStored, bTotal))
	}
	fmt.Fprintln(w)

	// Synthesis compression: ratio of entry_len to assistant_len for stored rows.
	fmt.Fprintf(w, "### Synthesis compression (stored rows only)\n\n")
	var compRatios []float64
	var totalAsstLen, totalEntryLen int64
	for _, r := range results {
		if r.Stage == "stored" && r.EntryLen > 0 && r.AssistantLen > 0 {
			ratio := float64(r.EntryLen) / float64(r.AssistantLen)
			compRatios = append(compRatios, ratio)
			totalAsstLen += int64(r.AssistantLen)
			totalEntryLen += int64(r.EntryLen)
		}
	}
	if len(compRatios) > 0 {
		sort.Float64s(compRatios)
		var sum float64
		for _, r := range compRatios {
			sum += r
		}
		mean := sum / float64(len(compRatios))
		median := compRatios[len(compRatios)/2]
		fmt.Fprintf(w, "- **Samples:** %d stored rows with synthesized entry\n", len(compRatios))
		fmt.Fprintf(w, "- **Mean entry/response ratio:** %.2f (entries are %.0f%% the size of original responses)\n", mean, mean*100)
		fmt.Fprintf(w, "- **Median ratio:** %.2f\n", median)
		fmt.Fprintf(w, "- **Total input:** %s → **Total output:** %s (%.1f%% compression)\n",
			humanBytes(totalAsstLen), humanBytes(totalEntryLen),
			(1-float64(totalEntryLen)/float64(totalAsstLen))*100)
	} else {
		fmt.Fprintln(w, "_No stored rows with synthesis data. Synthesizer may not be active._")
	}
	fmt.Fprintln(w)

	// ================================================================
	// Section 5: Latency Analysis
	// ================================================================
	fmt.Fprintf(w, "## 5. Latency Analysis\n\n")

	// Overall latency.
	var allLat []float64
	latByStage := map[string][]float64{}
	for _, r := range results {
		if r.Stage != "error" {
			ms := float64(r.LatencyMs)
			allLat = append(allLat, ms)
			latByStage[r.Stage] = append(latByStage[r.Stage], ms)
		}
	}

	if len(allLat) > 0 {
		sort.Float64s(allLat)
		fmt.Fprintf(w, "### Overall\n\n")
		fmt.Fprintln(w, "| p50 | p90 | p95 | p99 | mean | min | max |")
		fmt.Fprintln(w, "|-----|-----|-----|-----|------|-----|-----|")
		fmt.Fprintf(w, "| %.0f ms | %.0f ms | %.0f ms | %.0f ms | %.0f ms | %.0f ms | %.0f ms |\n\n",
			percentile(allLat, 50), percentile(allLat, 90), percentile(allLat, 95),
			percentile(allLat, 99), mean(allLat), allLat[0], allLat[len(allLat)-1])

		// Per-stage latency.
		fmt.Fprintf(w, "### By pipeline stage\n\n")
		fmt.Fprintln(w, "| Stage | Count | p50 (ms) | p90 (ms) | p99 (ms) | Mean (ms) |")
		fmt.Fprintln(w, "|-------|-------|----------|----------|----------|-----------|")
		for _, st := range stageOrder {
			lats := latByStage[st]
			if len(lats) == 0 {
				continue
			}
			sort.Float64s(lats)
			fmt.Fprintf(w, "| %s | %d | %.0f | %.0f | %.0f | %.0f |\n",
				st, len(lats), percentile(lats, 50), percentile(lats, 90),
				percentile(lats, 99), mean(lats))
		}
		fmt.Fprintln(w)
	}

	// Latency by assistant response length.
	fmt.Fprintf(w, "### Latency by response length (stored rows)\n\n")
	fmt.Fprintln(w, "| Length bucket | Count | p50 (ms) | p90 (ms) | Mean (ms) |")
	fmt.Fprintln(w, "|-------------|-------|----------|----------|-----------|")
	for _, b := range buckets {
		var lats []float64
		for _, r := range results {
			if r.Stage == "stored" && r.AssistantLen >= b.lo && r.AssistantLen < b.hi {
				lats = append(lats, float64(r.LatencyMs))
			}
		}
		if len(lats) == 0 {
			continue
		}
		sort.Float64s(lats)
		fmt.Fprintf(w, "| %s chars | %d | %.0f | %.0f | %.0f |\n",
			b.label, len(lats), percentile(lats, 50), percentile(lats, 90), mean(lats))
	}
	fmt.Fprintln(w)

	// ================================================================
	// Section 6: Write Pipeline Detail (stored rows)
	// ================================================================
	fmt.Fprintf(w, "## 6. Write Pipeline Detail (stored rows)\n\n")

	var wpStored, wpDupes, wpFiltered, wpExtended, wpMerged int
	for _, r := range results {
		if r.Stage == "stored" {
			wpStored += r.WritePipeStored
			wpDupes += r.WritePipeDupes
			wpFiltered += r.WritePipeFiltered
			wpExtended += r.WritePipeExtended
			wpMerged += r.WritePipeMerged
		}
	}
	fmt.Fprintf(w, "Across all %d stored rows, the write pipeline reported:\n\n", stored)
	fmt.Fprintln(w, "| Metric | Count | Meaning |")
	fmt.Fprintln(w, "|--------|-------|---------|")
	fmt.Fprintf(w, "| Chunks stored | %d | Individual memories inserted into MongoDB |\n", wpStored)
	fmt.Fprintf(w, "| Duplicates skipped | %d | Near-duplicate chunks (cosine >= 0.92) |\n", wpDupes)
	fmt.Fprintf(w, "| Noise filtered | %d | Chunks below noise threshold |\n", wpFiltered)
	fmt.Fprintf(w, "| Source extended | %d | Chunks extending existing source memories |\n", wpExtended)
	fmt.Fprintf(w, "| Topic merged | %d | Chunks absorbed into topic groups |\n", wpMerged)
	fmt.Fprintln(w)

	// Dedup accumulation: how many dupes build up over the run.
	fmt.Fprintf(w, "### Dedup accumulation over time\n\n")
	fmt.Fprintf(w, "Cumulative duplicate count by row number (every 10%% of run).\n\n")
	fmt.Fprintln(w, "| Row # | Cumulative dupes |")
	fmt.Fprintln(w, "|-------|-----------------|")
	cumDupes := 0
	step := total / 10
	if step < 1 {
		step = 1
	}
	for i, r := range results {
		cumDupes += r.WritePipeDupes
		if (i+1)%step == 0 || i == total-1 {
			fmt.Fprintf(w, "| %d | %d |\n", i+1, cumDupes)
		}
	}
	fmt.Fprintln(w)

	// ================================================================
	// Section 7: Search Quality Probes
	// ================================================================
	fmt.Fprintf(w, "## 7. Search Quality Probes\n\n")
	fmt.Fprintf(w, "Queries run after ingestion to test retrieval quality.\n\n")
	fmt.Fprintln(w, "| Query | Top Score | Hits | Top Result Preview |")
	fmt.Fprintln(w, "|-------|-----------|------|-------------------|")
	for _, p := range probes {
		preview := strings.ReplaceAll(p.Preview, "|", "\\|")
		preview = strings.ReplaceAll(preview, "\n", " ")
		fmt.Fprintf(w, "| %s | %.3f | %d | %s |\n",
			truncate(p.Query, 50), p.TopScore, p.NumHits, truncate(preview, 80))
	}
	fmt.Fprintln(w)

	avgScore := 0.0
	if len(probes) > 0 {
		var sum float64
		for _, p := range probes {
			sum += p.TopScore
		}
		avgScore = sum / float64(len(probes))
	}
	fmt.Fprintf(w, "**Average top score across all probes:** %.3f\n\n", avgScore)

	// ================================================================
	// Section 8: Rejection Store Analysis
	// ================================================================
	fmt.Fprintf(w, "## 8. Rejection Store\n\n")
	fmt.Fprintf(w, "- **Total rejections buffered:** %d / %d capacity\n", rejStats.Total, rejStats.Capacity)
	if len(rejStats.ByStage) > 0 {
		fmt.Fprintln(w, "\n| Stage | Count |")
		fmt.Fprintln(w, "|-------|-------|")
		for stage, n := range rejStats.ByStage {
			fmt.Fprintf(w, "| %s | %d |\n", stage, n)
		}
	}
	fmt.Fprintf(w, "\nThe rejection store feeds the adaptive noise scorer. After %d entries,\n", rejStats.Total)
	fmt.Fprintf(w, "the scorer has a richer model of what \"noise\" looks like in real conversations.\n\n")

	// ================================================================
	// Section 9: Error Analysis
	// ================================================================
	errorCount := stageCount["error"]
	if errorCount > 0 {
		fmt.Fprintf(w, "## 9. Errors\n\n")
		fmt.Fprintf(w, "**Total errors:** %d / %d (%.1f%%)\n\n", errorCount, total, pct(errorCount, total))
		// Show first 10 unique errors.
		errorSamples := map[string]int{}
		for _, r := range results {
			if r.Stage == "error" && r.Error != "" {
				key := truncate(r.Error, 100)
				errorSamples[key]++
			}
		}
		fmt.Fprintln(w, "| Error | Count |")
		fmt.Fprintln(w, "|-------|-------|")
		i := 0
		for errMsg, n := range errorSamples {
			if i >= 10 {
				break
			}
			fmt.Fprintf(w, "| %s | %d |\n", errMsg, n)
			i++
		}
		fmt.Fprintln(w)
	}

	// ================================================================
	// Section 10: Summary & Recommendations
	// ================================================================
	fmt.Fprintf(w, "## 10. Summary\n\n")
	fmt.Fprintln(w, "| Metric | Value |")
	fmt.Fprintln(w, "|--------|-------|")
	fmt.Fprintf(w, "| Rows processed | %d |\n", total)
	fmt.Fprintf(w, "| Store rate | %.1f%% |\n", pct(stored, total))
	fmt.Fprintf(w, "| Filter rate | %.1f%% |\n", pct(totalFiltered, total))
	if classCount["explanation"] > 0 {
		fmt.Fprintf(w, "| Explanation leak rate | %.1f%% |\n", pct(crossTab["explanation"]["stored"], classCount["explanation"]))
	}
	if classCount["substantive"] > 0 {
		substFiltered := classCount["substantive"] - crossTab["substantive"]["stored"]
		fmt.Fprintf(w, "| Substantive false-positive rate | %.1f%% |\n", pct(substFiltered, classCount["substantive"]))
	}
	fmt.Fprintf(w, "| Memories created | +%d |\n", memsAfter-memsBefore)
	fmt.Fprintf(w, "| Avg search probe score | %.3f |\n", avgScore)
	fmt.Fprintf(w, "| Median latency | %.0f ms |\n", percentile(allLat, 50))
	if len(compRatios) > 0 {
		fmt.Fprintf(w, "| Median synthesis compression | %.0f%% |\n",
			(1-compRatios[len(compRatios)/2])*100)
	}
	fmt.Fprintln(w)

	// Cleanup.
	if cleanupCount >= 0 {
		fmt.Fprintf(w, "\n**Cleanup:** Deleted %d benchmark memories (source=`%s`).\n", cleanupCount, source)
	} else {
		fmt.Fprintf(w, "\n**Cleanup:** Benchmark memories retained (source=`%s`). Delete manually if desired.\n", source)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p/100)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var s float64
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals))
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// loadLocalJSONL reads rows from a dataset.jsonl file in dataDir.
// If limit > 0 it stops after that many rows; if limit <= 0 it reads all.
func loadLocalJSONL(dataDir string, limit int) ([]hfRow, error) {
	p := filepath.Join(dataDir, "dataset.jsonl")
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	defer f.Close()

	var rows []hfRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024) // 4 MB lines
	for scanner.Scan() {
		if limit > 0 && len(rows) >= limit {
			break
		}
		var row hfRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue // skip malformed lines
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	return rows, nil
}
