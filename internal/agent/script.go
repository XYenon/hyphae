package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/aleksanaa/hyphae/internal/session"
	"github.com/aleksanaa/hyphae/internal/strutil"
	starlarkjson "github.com/aleksanaa/hyphae/internal/third_party/starlark/lib/json"
	starlarkmath "github.com/aleksanaa/hyphae/internal/third_party/starlark/lib/math"
	starlarktime "github.com/aleksanaa/hyphae/internal/third_party/starlark/lib/time"
	"github.com/aleksanaa/hyphae/internal/third_party/starlark/starlark"
	"github.com/aleksanaa/hyphae/internal/third_party/starlark/syntax"
	"github.com/boyter/gocodewalker"
)

// ── Starlark built-in implementations ────────────────────────────────────────

func starlarkReadFile(_ context.Context, args map[string]any, workDir string) (starlark.Value, error) {
	path := resolvePath(str(args, "path"), workDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)

	offset := 1
	if o, ok := args["offset"].(float64); ok && o >= 1 {
		offset = int(o)
	}
	limit := 2000
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	idx := offset - 1
	if idx >= total {
		return starlark.String(fmt.Sprintf("(past end of file — file has %d lines)", total)), nil
	}
	end := idx + limit
	if end > total {
		end = total
	}

	var sb strings.Builder
	for _, line := range lines[idx:end] {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if end < total {
		fmt.Fprintf(&sb, "\n(%d more lines — use offset=%d to continue)", total-end, end+1)
	}
	return starlark.String(sb.String()), nil
}

func starlarkWriteFile(_ context.Context, args map[string]any, workDir string) (starlark.Value, error) {
	path := resolvePath(str(args, "path"), workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	content := str(args, "content")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return starlark.String(fmt.Sprintf("wrote %d bytes to %s", len(content), path)), nil
}

func starlarkEditFile(_ context.Context, args map[string]any, workDir string) (starlark.Value, error) {
	path := resolvePath(str(args, "path"), workDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	edits, _ := args["edits"].([]any)
	if len(edits) == 0 {
		return nil, fmt.Errorf("edits must be a non-empty array")
	}
	content := string(b)
	for i, e := range edits {
		em, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edit %d: invalid format", i)
		}
		oldStr := str(em, "old_string")
		newStr := str(em, "new_string")
		count := strings.Count(content, oldStr)
		if count == 0 {
			return nil, fmt.Errorf("edit %d: old_string not found", i)
		}
		if count > 1 {
			return nil, fmt.Errorf("edit %d: old_string appears %d times — add more context to make it unique", i, count)
		}
		content = strings.Replace(content, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return starlark.String(fmt.Sprintf("edited %s (%d replacements)", path, len(edits))), nil
}

func starlarkListDirectory(_ context.Context, args map[string]any, workDir string) (starlark.Value, error) {
	p := str(args, "path")
	if p == "" {
		p = workDir
	} else {
		p = resolvePath(p, workDir)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	elems := make([]starlark.Value, len(entries))
	for i, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		elems[i] = starlark.String(name)
	}
	return starlark.NewList(elems), nil
}

func starlarkRunShell(ctx context.Context, args map[string]any, workDir string) (starlark.Value, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", str(args, "command"))
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return starlark.String(string(out) + "\n[exit error: " + err.Error() + "]"), nil
	}
	return starlark.String(out), nil
}

func starlarkWebFetch(ctx context.Context, args map[string]any, _ string) (starlark.Value, error) {
	rawURL := str(args, "url")
	if rawURL == "" {
		return nil, fmt.Errorf("url is required")
	}
	format := str(args, "format")
	if format == "" {
		format = "markdown"
	}
	timeout := 0
	if t, ok := args["timeout"].(float64); ok {
		timeout = int(t)
	}
	s, err := fetchURL(ctx, rawURL, format, timeout)
	if err != nil {
		return nil, err
	}
	return starlark.String(s), nil
}

func starlarkWebSearch(ctx context.Context, args map[string]any, _ string) (starlark.Value, error) {
	query := str(args, "query")
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	max := ddgMaxResults
	if m, ok := args["max_results"].(float64); ok && m > 0 {
		max = int(m)
		if max > ddgMaxResults {
			max = ddgMaxResults
		}
	}
	results, err := duckduckgoSearch(ctx, query, max)
	if err != nil {
		return nil, err
	}
	elems := make([]starlark.Value, 0, len(results))
	for _, r := range results {
		d := new(starlark.Dict)
		d.SetKey(starlark.String("title"), starlark.String(r.Title))     //nolint:errcheck
		d.SetKey(starlark.String("url"), starlark.String(r.URL))         //nolint:errcheck
		d.SetKey(starlark.String("snippet"), starlark.String(r.Snippet)) //nolint:errcheck
		elems = append(elems, d)
	}
	return starlark.NewList(elems), nil
}

func starlarkSearchFiles(ctx context.Context, args map[string]any, workDir string) (starlark.Value, error) {
	pathGlob := str(args, "path_glob")
	if pathGlob == "" {
		return nil, fmt.Errorf(`path_glob is required (e.g. "**", "**/*.go", "src/**", "~/notes/**")`)
	}
	caseSensitive, _ := args["case_sensitive"].(bool)

	// A bare glob (no "/") matches the base name at any depth under the working
	// directory; otherwise the glob is resolved to an absolute path and matched
	// against absolute file paths, so it can reach any readable directory.
	baseOnly := !strings.Contains(pathGlob, "/")
	globInput := pathGlob
	if !baseOnly {
		globInput = filepath.ToSlash(resolvePath(pathGlob, workDir))
	}
	globRe, err := compileGlob(globInput, caseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid path_glob: %w", err)
	}

	var contentRe *regexp.Regexp
	if cr := str(args, "content_regex"); cr != "" {
		contentRe, err = compileRegex(cr, caseSensitive)
		if err != nil {
			return nil, fmt.Errorf("invalid content_regex: %w", err)
		}
	}

	// exclude_glob drops matching files, matched like path_glob: a bare glob (no
	// "/") against the base name, otherwise against the absolute slash path.
	var excludeRe *regexp.Regexp
	excludeBaseOnly := false
	if eg := str(args, "exclude_glob"); eg != "" {
		excludeBaseOnly = !strings.Contains(eg, "/")
		excludeInput := eg
		if !excludeBaseOnly {
			excludeInput = filepath.ToSlash(resolvePath(eg, workDir))
		}
		excludeRe, err = compileGlob(excludeInput, caseSensitive)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude_glob: %w", err)
		}
	}

	root := searchGlobRoot(pathGlob, workDir)

	searchCtx, cancelSearch := context.WithCancel(ctx)
	defer cancelSearch()

	fileQueue := make(chan *gocodewalker.File, 1000)
	walker := gocodewalker.NewParallelFileWalker([]string{root}, fileQueue)
	walker.ExcludeDirectory = []string{".git", ".hg", ".svn"}

	go func() {
		<-searchCtx.Done()
		walker.Terminate()
	}()
	go func() { _ = walker.Start() }()

	var elems []starlark.Value

	for f := range fileQueue {
		if searchCtx.Err() != nil {
			break
		}

		slashPath := filepath.ToSlash(f.Location)
		target := slashPath
		if baseOnly {
			target = f.Filename
		}
		if !globRe.MatchString(target) {
			continue
		}

		if excludeRe != nil {
			exTarget := slashPath
			if excludeBaseOnly {
				exTarget = f.Filename
			}
			if excludeRe.MatchString(exTarget) {
				continue
			}
		}

		// Display path: working-dir-relative when inside it, absolute otherwise.
		rel, err := filepath.Rel(workDir, f.Location)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = f.Location
		}

		// No content_regex: report the matched file itself.
		if contentRe == nil {
			d := new(starlark.Dict)
			d.SetKey(starlark.String("file"), starlark.String(rel)) //nolint:errcheck
			elems = append(elems, d)
			continue
		}

		content, err := os.ReadFile(f.Location)
		if err != nil || len(content) == 0 {
			continue
		}

		// Skip binary files.
		check := content
		if len(check) > 10_000 {
			check = check[:10_000]
		}
		if bytes.IndexByte(check, 0) != -1 {
			continue
		}

		for lineNum, line := range strings.Split(string(content), "\n") {
			if contentRe.MatchString(line) {
				d := new(starlark.Dict)
				d.SetKey(starlark.String("file"), starlark.String(rel))                              //nolint:errcheck
				d.SetKey(starlark.String("line"), starlark.MakeInt(lineNum+1))                       //nolint:errcheck
				d.SetKey(starlark.String("content"), starlark.String(strings.TrimRight(line, "\r"))) //nolint:errcheck
				elems = append(elems, d)
			}
		}
	}

	return starlark.NewList(elems), nil
}

// compileRegex compiles a user-supplied regex, applying case-insensitivity
// unless caseSensitive is set.
func compileRegex(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// compileGlob translates a shell-style glob into a whole-string-anchored regexp.
// "**" spans path separators ("**/" also matches zero leading segments); "*" and
// "?" stop at "/". Literal text is regexp-escaped.
func compileGlob(glob string, caseSensitive bool) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++ // consume the second '*'
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++ // consume the '/': "**/" matches zero or more segments
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	reStr := b.String()
	if !caseSensitive {
		reStr = "(?i)" + reStr
	}
	return regexp.Compile(reStr)
}

// ── Script execution ──────────────────────────────────────────────────────────

// scriptTool pairs a Starlark built-in name with its Go implementation.
// firstParam is the parameter name that maps to the first positional argument.
// The verb/noun fields drive live status updates and the post-round summary.
type scriptTool struct {
	name       string
	firstParam string
	fn         func(context.Context, map[string]any, string) (starlark.Value, error)
	activeVerb string // gerund shown during execution: "reading", "editing", "running"
	wantVerb   string // infinitive shown before approval: "read", "edit", "run"
	doneVerb   string // past tense for summary: "read", "edited", "ran"
	doneNounP  string // plural noun for count>1: "files", "commands", "queries"
}

var scriptTools = []scriptTool{
	{"read_file", "path", starlarkReadFile, "is reading", "read", "read", "files"},
	{"write_file", "path", starlarkWriteFile, "is writing", "write", "wrote", "files"},
	{"edit_file", "path", starlarkEditFile, "is editing", "edit", "edited", "files"},
	{"list_directory", "path", starlarkListDirectory, "is listing", "list", "listed", "dirs"},
	{"run_shell", "command", starlarkRunShell, "is running", "run", "ran", "commands"},
	{"web_fetch", "url", starlarkWebFetch, "is fetching", "fetch", "fetched", "URLs"},
	{"web_search", "query", starlarkWebSearch, "is searching", "search", "searched", "queries"},
	{"search_files", "path_glob", starlarkSearchFiles, "is searching files", "search", "searched", "globs"},
}

// toolDisplayTarget extracts a short display string for the primary argument.
// File paths are shortened to the base name; other values are truncated.
func toolDisplayTarget(firstParam string, argsMap map[string]any) string {
	raw := str(argsMap, firstParam)
	if firstParam == "path" {
		b := filepath.Base(raw)
		if b == "" || b == "." {
			return "."
		}
		return b
	}
	return strutil.Truncate(raw, 30)
}

// maxRunOutputBytes caps the print() output returned from a run call. Beyond it
// the output is dropped (a notice is returned instead) so a runaway script can't
// exhaust the context; the script's namespace persists across calls, so the model
// can still fetch the data in smaller pieces on a later run. ~5000 lines of code.
const maxRunOutputBytes = 256 * 1024

// runScript executes a Starlark program with all agent operations available as
// built-in functions. The script's print() output is returned as the result.
// Tools requiring user approval pause mid-script for confirmation.
// anyToolsRan reports whether any built-in tool was invoked during execution.
func runScript(ctx context.Context, ch chan<- Event, argsJSON, workDir string, ns starlark.StringDict, grants *grantSet) (output string, isErr bool, anyToolsRan bool) {
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Code == "" {
		return "code is required", true, false
	}

	var sb strings.Builder
	var counter atomic.Int64

	thread := &starlark.Thread{
		Print: func(_ *starlark.Thread, msg string) {
			sb.WriteString(msg)
			sb.WriteByte('\n')
		},
	}

	// Cancel the Starlark thread when the agent context expires.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("cancelled")
		case <-done:
		}
	}()

	var called atomic.Bool
	env := buildScriptEnv(ctx, ch, workDir, &counter, &called, grants)

	// Merge persisted namespace into predeclared (built-ins win over namespace).
	predeclared := make(starlark.StringDict, len(env)+len(ns))
	for k, v := range ns {
		predeclared[k] = v
	}
	for k, v := range env {
		predeclared[k] = v
	}

	thread.SetMaxExecutionSteps(1_000_000_000)
	opts := &syntax.FileOptions{TopLevelControl: true, GlobalReassign: true, While: true, Set: true, Recursion: true, REPL: true}
	globals, err := starlark.ExecFileOptions(opts, thread, "<run>", args.Code, predeclared)

	// Persist new globals back into the session namespace (skip built-in names).
	for k, v := range globals {
		if _, isBuiltin := env[k]; !isBuiltin {
			ns[k] = v
		}
	}

	// Drop over-large output (keeping any error trace, which is small) so it can't
	// exhaust the context. The namespace survives, so the model can retrieve the
	// data in smaller pieces on a follow-up run.
	out := sb.String()
	if sb.Len() > maxRunOutputBytes {
		out = fmt.Sprintf("Error: the run output was not returned because it is %d bytes, over the %d-byte limit — returning it would exhaust the context. Any variables, functions, and other names the script defined are still stored in your namespace; retrieve the data another way in a follow-up run (e.g. slice or filter it, or print a summary or count).", sb.Len(), maxRunOutputBytes)
	}

	if err != nil {
		var evalErr *starlark.EvalError
		var parseErr syntax.Error
		if errors.As(err, &evalErr) {
			out += formatEvalError(evalErr, args.Code)
		} else if errors.As(err, &parseErr) {
			out += formatParseError(parseErr, args.Code)
		} else {
			out += err.Error()
		}
		return strings.TrimRight(out, "\n"), true, called.Load()
	}

	out = strings.TrimRight(out, "\n")
	if out == "" {
		return "(done)", false, called.Load()
	}
	return out, false, called.Load()
}

// formatParseError formats a Starlark syntax.Error with the offending source line.
func formatParseError(e syntax.Error, src string) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "Parse error at %s: %s", e.Pos, e.Msg)
	srcLines := strings.Split(src, "\n")
	line := int(e.Pos.Line)
	if line > 0 && line <= len(srcLines) {
		fmt.Fprintf(&buf, "\n    %s", srcLines[line-1])
		if col := int(e.Pos.Col); col > 0 {
			fmt.Fprintf(&buf, "\n    %s^", strings.Repeat(" ", col-1))
		}
	}
	return buf.String()
}

// formatEvalError formats a Starlark EvalError with source lines and caret indicators.
func formatEvalError(e *starlark.EvalError, src string) string {
	srcLines := strings.Split(src, "\n")
	var buf strings.Builder

	stack := e.CallStack
	suffix := ""
	if n := len(stack); n > 0 && stack[n-1].Pos.Filename() == "<builtin>" {
		suffix = " in " + stack[n-1].Name
		stack = stack[:n-1]
	}

	if len(stack) > 0 {
		buf.WriteString("Traceback (most recent call last):\n")
	}
	for _, fr := range stack {
		fmt.Fprintf(&buf, "  %s: in %s\n", fr.Pos, fr.Name)
		line := int(fr.Pos.Line)
		if fr.Pos.Filename() == "<run>" && line > 0 && line <= len(srcLines) {
			fmt.Fprintf(&buf, "    %s\n", srcLines[line-1])
			if col := int(fr.Pos.Col); col > 0 {
				fmt.Fprintf(&buf, "    %s^\n", strings.Repeat(" ", col-1))
			}
		}
	}
	fmt.Fprintf(&buf, "Error%s: %s", suffix, e.Msg)
	return buf.String()
}

// buildScriptEnv builds the predeclared Starlark environment: math and time
// modules plus every operation wrapped as a built-in function.
// called is set to true the first time any status event is emitted (i.e. a tool was invoked).
func buildScriptEnv(ctx context.Context, ch chan<- Event, workDir string, counter *atomic.Int64, called *atomic.Bool, grants *grantSet) starlark.StringDict {
	emitStatusEvent := func(ev session.StatusEvent) {
		called.Store(true)
		select {
		case ch <- Event{Type: EventStatusUpdate, StatusEvent: ev}:
		case <-ctx.Done():
		}
	}
	env := starlark.StringDict{
		"json": starlarkjson.Module,
		"math": starlarkmath.Module,
		"time": starlarktime.Module,
		// divmod(a, b) returns (a//b, a%b) as a tuple, matching Python.
		"divmod": starlark.NewBuiltin("divmod", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 2 {
				return nil, fmt.Errorf("divmod: got %d arguments, want 2", len(args))
			}
			a, aok := starlark.AsFloat(args[0])
			b, bok := starlark.AsFloat(args[1])
			if !aok || !bok {
				return nil, fmt.Errorf("divmod: arguments must be numbers")
			}
			if b == 0 {
				return nil, fmt.Errorf("divmod: division by zero")
			}
			q := math.Trunc(a / b)
			r := a - q*b
			// Return ints when both inputs are integers.
			if _, aIsInt := args[0].(starlark.Int); aIsInt {
				if _, bIsInt := args[1].(starlark.Int); bIsInt {
					qi, _ := starlark.NumberToInt(starlark.Float(q))
					ri, _ := starlark.NumberToInt(starlark.Float(r))
					return starlark.Tuple{qi, ri}, nil
				}
			}
			return starlark.Tuple{starlark.Float(q), starlark.Float(r)}, nil
		}),
		// Shadow the built-in round() with a two-argument version: round(x, ndigits=0).
		"round": starlark.NewBuiltin("round", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) < 1 || len(args) > 2 {
				return nil, fmt.Errorf("round: got %d arguments, want 1 or 2", len(args))
			}
			f, ok := starlark.AsFloat(args[0])
			if !ok {
				return nil, fmt.Errorf("round: first argument must be a number, got %s", args[0].Type())
			}
			if len(args) == 1 {
				// Match Python: round(x) returns int.
				return starlark.NumberToInt(starlark.Float(math.Round(f)))
			}
			n, ok := args[1].(starlark.Int)
			if !ok {
				return nil, fmt.Errorf("round: ndigits must be an integer, got %s", args[1].Type())
			}
			ndigits, _ := n.Int64()
			factor := math.Pow(10, float64(ndigits))
			return starlark.Float(math.Round(f*factor) / factor), nil
		}),
	}

	for _, t := range scriptTools {
		toolName := t.name
		toolFn := t.fn
		firstParam := t.firstParam
		activeVerb := t.activeVerb
		wantVerb := t.wantVerb
		doneVerb := t.doneVerb
		doneNounP := t.doneNounP
		env[toolName] = starlark.NewBuiltin(toolName, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			argsMap := kwargsToMap(kwargs)
			if len(args) > 0 && firstParam != "" {
				if _, exists := argsMap[firstParam]; !exists {
					argsMap[firstParam] = starlarkToGo(args[0])
				}
			}
			target := toolDisplayTarget(firstParam, argsMap)
			// A call within the session's permissions runs immediately. Anything
			// else pauses for the user to approve it (and requires reasoning).
			if approvalNeeded(toolName, argsMap, workDir, grants) {
				reasoning := strings.TrimSpace(str(argsMap, "reasoning"))
				if reasoning == "" {
					return nil, fmt.Errorf("Add a reasoning= field to the kwargs of %s, alongside its other arguments (not to the run() call), explaining why you need this — e.g. %s(..., reasoning=\"comparing against the upstream config outside my working dir\"). This location is outside your current permissions, and the reasoning= field is what lets the call go through. For repeated access to the same place you may instead call request_access(type=, target=, reasoning=) once to gain standing permission — type=\"readonly\" to keep reading one out-of-scope location, type=\"web_fetch\" to fetch several URLs under one prefix (not a one-off fetch), type=\"readwrite\" only when the user has explicitly handed you full control of a directory or project", toolName, toolName)
				}
				delete(argsMap, "reasoning") // shown as its own field, never listed as an arg
				te := &ToolEvent{
					CallID:    fmt.Sprintf("script:%d", counter.Add(1)),
					Name:      toolName,
					Args:      argsMap,
					Reasoning: reasoning,
				}
				var diffErr error
				te.FilePath, te.DiffPatch, diffErr = computeDiffForApproval(toolName, argsMap, workDir)
				if diffErr != nil {
					return nil, diffErr
				}
				if wantVerb != "" {
					emitStatusEvent(session.StatusEvent{Kind: session.StatusEventWants, Verb: wantVerb, Target: target})
				}
				respCh := make(chan ApprovalResult, 1)
				select {
				case ch <- Event{Type: EventToolApproval, Tool: te, RespCh: respCh}:
				case <-ctx.Done():
					return nil, fmt.Errorf("cancelled")
				}
				var approval ApprovalResult
				select {
				case approval = <-respCh:
				case <-ctx.Done():
					return nil, fmt.Errorf("cancelled")
				}
				if !approval.Allowed {
					emitStatusEvent(session.StatusEvent{Kind: session.StatusEventRefused, Verb: wantVerb, Target: target})
					msg := "denied by user"
					if approval.DenyReason != "" {
						msg += ": " + approval.DenyReason
					}
					return nil, fmt.Errorf("%s", msg)
				}
			}
			if activeVerb != "" {
				emitStatusEvent(session.StatusEvent{Kind: session.StatusEventDoing, Verb: activeVerb, Target: target})
			}
			delete(argsMap, "reasoning") // never forwarded to the tool implementation
			result, err := toolFn(ctx, argsMap, workDir)
			if err == nil && doneVerb != "" {
				emitStatusEvent(session.StatusEvent{Kind: session.StatusEventDone, Verb: doneVerb, NounP: doneNounP, Target: target})
			} else if err != nil && doneVerb != "" {
				emitStatusEvent(session.StatusEvent{Kind: session.StatusEventFailed, Verb: doneVerb, NounP: doneNounP, Target: target})
			}
			return result, err
		})
	}

	// ask_user: emits EventSelectPrompt and blocks until the user replies.
	env["ask_user"] = starlark.NewBuiltin("ask_user", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		m := kwargsToMap(kwargs)
		if len(args) > 0 {
			if _, exists := m["question"]; !exists {
				m["question"] = starlarkToGo(args[0])
			}
		}
		question, _ := m["question"].(string)
		target := strutil.Truncate(question, 30)
		emitStatusEvent(session.StatusEvent{Kind: session.StatusEventDoing, Verb: "is asking", Target: target})
		te := &ToolEvent{
			CallID:         fmt.Sprintf("script:%d", counter.Add(1)),
			Name:           "ask_user",
			SelectQuestion: question,
			SelectOptions:  toStringSlice(m["options"]),
		}
		respCh := make(chan string, 1)
		select {
		case ch <- Event{Type: EventSelectPrompt, Tool: te, SelectRespCh: respCh}:
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled")
		}
		var answer string
		select {
		case answer = <-respCh:
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled")
		}
		emitStatusEvent(session.StatusEvent{Kind: session.StatusEventDone, Verb: "asked", NounP: "questions", Target: target})
		return starlark.String(answer), nil
	})

	// request_access: ask the user to widen access for the rest of the session.
	// Emits an approval prompt; on allow, records a prefix-based grant so later
	// reads (readonly), writes (readwrite), or fetches (web_fetch) under that
	// scope proceed without asking again.
	env["request_access"] = starlark.NewBuiltin("request_access", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		m := kwargsToMap(kwargs)
		if len(args) > 0 {
			if _, exists := m["target"]; !exists {
				m["target"] = starlarkToGo(args[0])
			}
		}
		kind, _ := m["type"].(string)
		target, _ := m["target"].(string)
		reasoning := strings.TrimSpace(str(m, "reasoning"))
		switch kind {
		case "readonly", "readwrite", "web_fetch":
		default:
			return nil, fmt.Errorf(`request_access: type= must be "readonly", "readwrite", or "web_fetch"`)
		}
		if target == "" {
			return nil, fmt.Errorf("request_access: target is required (a directory path, or a URL prefix for web_fetch)")
		}
		if reasoning == "" {
			return nil, fmt.Errorf("request_access: reasoning= is required — explain why you need this access")
		}
		scope := grantScope(kind, target, workDir)

		te := &ToolEvent{
			CallID:    fmt.Sprintf("script:%d", counter.Add(1)),
			Name:      "request_access",
			Args:      map[string]any{"type": kind, "target": scope},
			Reasoning: reasoning,
		}
		emitStatusEvent(session.StatusEvent{Kind: session.StatusEventWants, Verb: "access", Target: scope})
		respCh := make(chan ApprovalResult, 1)
		select {
		case ch <- Event{Type: EventToolApproval, Tool: te, RespCh: respCh}:
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled")
		}
		var approval ApprovalResult
		select {
		case approval = <-respCh:
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled")
		}
		if !approval.Allowed {
			emitStatusEvent(session.StatusEvent{Kind: session.StatusEventRefused, Verb: "access", Target: scope})
			msg := "denied by user"
			if approval.DenyReason != "" {
				msg += ": " + approval.DenyReason
			}
			return nil, fmt.Errorf("%s", msg)
		}
		granted := grants.grant(kind, target, workDir)
		emitStatusEvent(session.StatusEvent{Kind: session.StatusEventDone, Verb: "granted", NounP: "grants", Target: scope})
		return starlark.String(fmt.Sprintf("granted %s access to %s for the rest of this session", kind, granted)), nil
	})

	return env
}

// ── Argument conversion ───────────────────────────────────────────────────────

func kwargsToMap(kwargs []starlark.Tuple) map[string]any {
	m := make(map[string]any, len(kwargs))
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key != "" {
			m[key] = starlarkToGo(kv[1])
		}
	}
	return m
}

func starlarkToGo(v starlark.Value) any {
	switch v := v.(type) {
	case starlark.String:
		return string(v)
	case starlark.Int:
		n, _ := v.Int64()
		return float64(n)
	case starlark.Float:
		return float64(v)
	case starlark.Bool:
		return bool(v)
	case *starlark.List:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = starlarkToGo(v.Index(i))
		}
		return out
	case *starlark.Dict:
		m := make(map[string]any, v.Len())
		for _, item := range v.Items() {
			k, _ := starlark.AsString(item[0])
			if k != "" {
				m[k] = starlarkToGo(item[1])
			}
		}
		return m
	}
	return v.String()
}

func toStringSlice(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
