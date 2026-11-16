// Package localstats aggregates token usage from Claude Code's local
// transcript files (<config dir>/projects/**/*.jsonl).
package localstats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tokens is a set of token counters.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
	// Total is Input + Output + CacheCreation + CacheRead.
	Total int64 `json:"total"`
}

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheCreation += o.CacheCreation
	t.CacheRead += o.CacheRead
	t.Total = t.Input + t.Output + t.CacheCreation + t.CacheRead
}

// Entry is one deduplicated assistant API response.
type Entry struct {
	Time      time.Time
	SessionID string
	Model     string
	Tokens    Tokens
}

// ModelUsage is per-model usage within a period.
type ModelUsage struct {
	Model    string  `json:"model"`
	Tokens   Tokens  `json:"tokens"`
	Messages int     `json:"messages"`
	Share    float64 `json:"share"` // fraction of the period's total tokens, 0..1
}

// Period aggregates entries in [Start, End).
type Period struct {
	Label    string       `json:"label"`
	Start    time.Time    `json:"start"`
	End      time.Time    `json:"end"`
	Tokens   Tokens       `json:"tokens"`
	Messages int          `json:"messages"`
	Sessions int          `json:"sessions"`
	Models   []ModelUsage `json:"models"`
}

// Stats is the local-usage summary.
type Stats struct {
	Today Period `json:"today"`
	Week  Period `json:"week"`
	// WeekFromAPI is true when Week uses the subscription's weekly window
	// rather than a rolling 7 days.
	WeekFromAPI  bool `json:"week_from_api"`
	FilesScanned int  `json:"files_scanned"`
	LinesSkipped int  `json:"lines_skipped"`
}

// DefaultDirs returns the transcript directories to scan. When the config
// dir was chosen explicitly (--config-dir or $CLAUDE_CONFIG_DIR) it may
// belong to a different account, so only <configDir>/projects is used.
// Otherwise ~/.claude/projects and ~/.config/claude/projects are scanned.
// Non-existent and duplicate directories are dropped.
func DefaultDirs(configDir string, explicit bool) []string {
	var cands []string
	if configDir != "" {
		cands = append(cands, filepath.Join(configDir, "projects"))
	}
	if !explicit {
		if home, err := os.UserHomeDir(); err == nil {
			cands = append(cands,
				filepath.Join(home, ".claude", "projects"),
				filepath.Join(home, ".config", "claude", "projects"))
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range cands {
		real, err := filepath.EvalSymlinks(d)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() || seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	return out
}

// ScanResult is the raw output of Scan.
type ScanResult struct {
	Entries      []Entry
	FilesScanned int
	LinesSkipped int
}

// findFiles lists *.jsonl files under dirs modified at or after since, in
// walk order. Symlinked files are judged by their target's mtime and
// symlinked directories are followed (each real directory and file is
// visited once, so link loops terminate).
func findFiles(dirs []string, since time.Time) ([]string, error) {
	var files []string
	visitedDirs := map[string]bool{}
	seenFiles := map[string]bool{}
	var walkErr error
	var walk func(root string)
	walk = func(root string) {
		real, err := filepath.EvalSymlinks(root)
		if err != nil || visitedDirs[real] {
			return
		}
		visitedDirs[real] = true
		err = filepath.WalkDir(real, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() && path != real {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if path != real {
					rp, err := filepath.EvalSymlinks(path)
					if err != nil || visitedDirs[rp] {
						return fs.SkipDir
					}
					visitedDirs[rp] = true
				}
				return nil
			}
			var info fs.FileInfo
			if d.Type()&fs.ModeSymlink != 0 {
				// os.Stat follows the link: a directory is walked on its
				// own, a file is judged by its target's mtime.
				info, err = os.Stat(path)
				if err != nil {
					return nil
				}
				if info.IsDir() {
					walk(path)
					return nil
				}
			} else if info, err = d.Info(); err != nil {
				return nil
			}
			if !info.Mode().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") || info.ModTime().Before(since) {
				return nil
			}
			key := path
			if rp, err := filepath.EvalSymlinks(path); err == nil {
				key = rp
			}
			if !seenFiles[key] {
				seenFiles[key] = true
				files = append(files, path)
			}
			return nil
		})
		if err != nil && walkErr == nil {
			walkErr = err
		}
	}
	for _, dir := range dirs {
		walk(dir)
	}
	return files, walkErr
}

// fileResult is one parsed transcript.
type fileResult struct {
	entries []keyedEntry
	skipped int
	ok      bool
}

type keyedEntry struct {
	key string // message.id + requestId; empty when the line had neither
	e   Entry
}

// Scan reads every *.jsonl file under dirs modified at or after since and
// returns deduplicated assistant entries with timestamps >= since.
//
// Claude Code writes one transcript line per content block, each repeating
// the same message.id/requestId and usage, so entries are keyed on that
// pair; when duplicates disagree the one with the most output tokens (the
// most complete streaming snapshot) wins.
//
// Files are parsed concurrently (JSON decoding dominates the cost) and
// merged in walk order, so the result is the same as a sequential scan.
func Scan(dirs []string, since time.Time) (ScanResult, error) {
	var res ScanResult
	files, walkErr := findFiles(dirs, since)
	results := make([]fileResult, len(files))
	workers := min(runtime.GOMAXPROCS(0), len(files))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = parseFile(files[i], since)
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	byKey := map[string]int{}
	for _, fr := range results {
		if !fr.ok {
			continue
		}
		res.FilesScanned++
		res.LinesSkipped += fr.skipped
		for _, ke := range fr.entries {
			if ke.key == "" {
				res.Entries = append(res.Entries, ke.e)
				continue
			}
			if i, ok := byKey[ke.key]; ok {
				if ke.e.Tokens.Output > res.Entries[i].Tokens.Output {
					res.Entries[i].Tokens = ke.e.Tokens
				}
				continue
			}
			byKey[ke.key] = len(res.Entries)
			res.Entries = append(res.Entries, ke.e)
		}
	}
	return res, walkErr
}

func parseFile(path string, since time.Time) fileResult {
	f, err := os.Open(path)
	if err != nil {
		return fileResult{}
	}
	defer f.Close()
	var fr fileResult
	byKey := map[string]int{}
	fr.skipped = scanReader(f, since, byKey, &fr.entries)
	fr.ok = true
	return fr
}

var (
	markAssistant = []byte(`"assistant"`)
	markUsage     = []byte(`"usage"`)
)

type rawLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// scanReader parses one transcript stream, appending new entries. It returns
// the number of malformed lines skipped. Lines of any length are handled.
func scanReader(r io.Reader, since time.Time, byKey map[string]int, out *[]keyedEntry) int {
	br := bufio.NewReaderSize(r, 256<<10)
	skipped := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if !processLine(line, since, byKey, out) {
				skipped++
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				skipped++
			}
			return skipped
		}
	}
}

// processLine returns false only for lines that look like assistant usage
// records but cannot be parsed.
func processLine(line []byte, since time.Time, byKey map[string]int, out *[]keyedEntry) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.Contains(line, markAssistant) || !bytes.Contains(line, markUsage) {
		return true
	}
	var rl rawLine
	if err := json.Unmarshal(line, &rl); err != nil {
		return false
	}
	if rl.Type != "assistant" || rl.Message == nil || rl.Message.Usage == nil {
		return true
	}
	model := strings.TrimSpace(rl.Message.Model)
	if model == "<synthetic>" {
		return true
	}
	ts, err := time.Parse(time.RFC3339Nano, rl.Timestamp)
	if err != nil {
		return false
	}
	if ts.Before(since) {
		return true
	}
	u := rl.Message.Usage
	tok := Tokens{Input: u.Input, Output: u.Output, CacheCreation: u.CacheCreation, CacheRead: u.CacheRead}
	tok.Total = tok.Input + tok.Output + tok.CacheCreation + tok.CacheRead
	e := Entry{Time: ts, SessionID: rl.SessionID, Model: model, Tokens: tok}

	if rl.Message.ID == "" && rl.RequestID == "" {
		*out = append(*out, keyedEntry{e: e})
		return true
	}
	key := rl.Message.ID + "\x00" + rl.RequestID
	if i, ok := byKey[key]; ok {
		if e.Tokens.Output > (*out)[i].e.Tokens.Output {
			(*out)[i].e.Tokens = e.Tokens
		}
		return true
	}
	byKey[key] = len(*out)
	*out = append(*out, keyedEntry{key: key, e: e})
	return true
}

// Summarize aggregates entries with start <= time < end.
func Summarize(entries []Entry, label string, start, end time.Time) Period {
	p := Period{Label: label, Start: start, End: end}
	sessions := map[string]bool{}
	models := map[string]*ModelUsage{}
	for _, e := range entries {
		if e.Time.Before(start) || !e.Time.Before(end) {
			continue
		}
		p.Messages++
		p.Tokens.add(e.Tokens)
		if e.SessionID != "" {
			sessions[e.SessionID] = true
		}
		name := e.Model
		if name == "" {
			name = "unknown"
		}
		m := models[name]
		if m == nil {
			m = &ModelUsage{Model: name}
			models[name] = m
		}
		m.Messages++
		m.Tokens.add(e.Tokens)
	}
	p.Sessions = len(sessions)
	p.Models = make([]ModelUsage, 0, len(models))
	for _, m := range models {
		if p.Tokens.Total > 0 {
			m.Share = float64(m.Tokens.Total) / float64(p.Tokens.Total)
		}
		p.Models = append(p.Models, *m)
	}
	sort.Slice(p.Models, func(i, j int) bool {
		if p.Models[i].Tokens.Total != p.Models[j].Tokens.Total {
			return p.Models[i].Tokens.Total > p.Models[j].Tokens.Total
		}
		return p.Models[i].Model < p.Models[j].Model
	})
	return p
}

// StartOfDay returns local midnight of t in loc.
func StartOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// Compute builds today's and the weekly summary. weekStart may be zero, in
// which case a rolling 7-day window is used.
func Compute(entries []Entry, now time.Time, loc *time.Location, weekStart time.Time) Stats {
	end := now.Add(time.Second)
	st := Stats{Today: Summarize(entries, "today", StartOfDay(now, loc), end)}
	if !weekStart.IsZero() && weekStart.Before(now) {
		st.Week = Summarize(entries, "week", weekStart, end)
		st.WeekFromAPI = true
	} else {
		st.Week = Summarize(entries, "7d", now.Add(-7*24*time.Hour), end)
	}
	return st
}
