package simplerouter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// claudePatchEdit is one same-length in-place rewrite of the Claude bundle.
// The replacement is padded with spaces up to length, so file offsets (which
// the bun bundle relies on) never move.
type claudePatchEdit struct {
	offset      int
	length      int
	replacement []byte
}

// claudeBinaryPatch locates its edits structurally — matching the shape of
// the minified code and deriving the build's identifier names from the match
// — so it keeps working across Claude Code releases that only reshuffle
// minified names.
type claudeBinaryPatch struct {
	name              string
	required          bool
	warnIfUnsupported bool
	disableEnv        string
	// markers are byte strings that all appear in the bundle only after the
	// patch has been fully applied.
	markers [][]byte
	// find returns every edit to apply on an unpatched bundle. ok=false means
	// this build's code no longer matches the expected shape (fall back to
	// the unpatched binary).
	find func(data []byte) (edits []claudePatchEdit, ok bool, err error)
}

var claudeBinaryPatches = []claudeBinaryPatch{
	{
		name:     "live-thinking",
		required: true,
		markers:  [][]byte{[]byte(liveThinkingPatchMarker)},
		find:     findLiveThinkingEdits,
	},
	{
		name:              "throughput-meter",
		warnIfUnsupported: true,
		disableEnv:        "SIMPLEROUTER_DISABLE_TOKEN_RATE",
		markers: [][]byte{
			[]byte(throughputStatePatchMarker),
			[]byte(throughputSpinnerPatchMarker),
			[]byte(throughputFinalPatchMarker),
		},
		find: findThroughputEdits,
	},
	{
		name:    "launch-version-marker",
		markers: [][]byte{[]byte(launchVersionPatchMarker)},
		find:    findLaunchVersionEdits,
	},
}

func (p claudeBinaryPatch) applied(data []byte) bool {
	if len(p.markers) == 0 {
		return false
	}
	for _, marker := range p.markers {
		if !bytes.Contains(data, marker) {
			return false
		}
	}
	return true
}

func (p claudeBinaryPatch) disabled() bool {
	return p.disableEnv != "" && strings.TrimSpace(os.Getenv(p.disableEnv)) != ""
}

// --- live-thinking patch ------------------------------------------------
//
// Claude Code's interactive stream dispatcher counts thinking_delta tokens
// for the spinner but throws the text away; thinking only reaches the screen
// when a completed block lands. The patch rewrites the thinking_delta case to
// also feed the delta text into the onStreamingThinking handler (the same one
// used when a finished block lands), which renders reasoning live.
//
// The minified case looks like (identifiers vary per build):
//
//	case"thinking_delta":{let{delta:d}=e.event;
//	  if("estimated_tokens"in d&&typeof d.estimated_tokens==="number")
//	    o?.({type:"thinking_progress",estimatedTokensDelta:d.estimated_tokens});
//	  else if("thinking"in d&&typeof d.thinking==="string"&&d.thinking.length>0)
//	    o?.({type:"thinking_progress",estimatedTokensDelta:_on(d.thinking)});
//	  return}
//
// and lives inside a function whose handlers object is identified by the
// destructuring `let{onSetStreamMode:...}=t` a few KB earlier.

// liveThinkingPatchMarker never occurs in unpatched bundles (verified against
// real builds); the replacement below embeds it.
const liveThinkingPatchMarker = `isStreaming:!0`

const claudeIdent = `[A-Za-z0-9_$]+`

var liveThinkingCaseRe = regexp.MustCompile(
	`case"thinking_delta":\{let\{delta:(` + claudeIdent + `)\}=(` + claudeIdent + `)\.event;` +
		`if\("estimated_tokens"in (` + claudeIdent + `)&&typeof (` + claudeIdent + `)\.estimated_tokens==="number"\)` +
		`(` + claudeIdent + `)\?\.\(\{type:"thinking_progress",estimatedTokensDelta:(` + claudeIdent + `)\.estimated_tokens\}\);` +
		`else if\("thinking"in (` + claudeIdent + `)&&typeof (` + claudeIdent + `)\.thinking==="string"&&(` + claudeIdent + `)\.thinking\.length>0\)` +
		`(` + claudeIdent + `)\?\.\(\{type:"thinking_progress",estimatedTokensDelta:(` + claudeIdent + `)\((` + claudeIdent + `)\.thinking\)\}\);return\}`)

var streamHandlersRe = regexp.MustCompile(`let\{[^{}]*?onSetStreamMode:[^{}]*?\}=(` + claudeIdent + `)`)

// liveThinkingHandlersWindow bounds the backwards scan from the matched case
// to the enclosing function's destructuring of the handlers object.
const liveThinkingHandlersWindow = 16 * 1024

func findLiveThinkingEdits(data []byte) ([]claudePatchEdit, bool, error) {
	var edits []claudePatchEdit
	for _, m := range liveThinkingCaseRe.FindAllSubmatchIndex(data, -1) {
		group := func(i int) string { return string(data[m[2*i]:m[2*i+1]]) }
		deltaVar, eventVar, callbackVar, estimatorFn := group(1), group(2), group(5), group(11)
		consistent := callbackVar == group(10)
		for _, i := range []int{3, 4, 6, 7, 8, 9, 12} {
			consistent = consistent && group(i) == deltaVar
		}
		if !consistent {
			continue
		}
		handlersVar, ok := findStreamHandlersVar(data, m[0])
		if !ok {
			continue
		}
		replacement := buildLiveThinkingReplacement(deltaVar, eventVar, callbackVar, estimatorFn, handlersVar)
		if len(replacement) > m[1]-m[0] {
			return nil, false, fmt.Errorf("Claude live-thinking replacement (%d bytes) exceeds target (%d bytes)", len(replacement), m[1]-m[0])
		}
		edits = append(edits, claudePatchEdit{offset: m[0], length: m[1] - m[0], replacement: replacement})
	}
	return edits, len(edits) > 0, nil
}

// findStreamHandlersVar resolves the enclosing function's handlers-object
// identifier by scanning backwards for the nearest destructuring that pulls
// onSetStreamMode out of it.
func findStreamHandlersVar(data []byte, before int) (string, bool) {
	start := before - liveThinkingHandlersWindow
	if start < 0 {
		start = 0
	}
	matches := streamHandlersRe.FindAllSubmatch(data[start:before], -1)
	if len(matches) == 0 {
		return "", false
	}
	return string(matches[len(matches)-1][1]), true
}

func buildLiveThinkingReplacement(deltaVar, eventVar, callbackVar, estimatorFn, handlersVar string) []byte {
	prevVar := "_" + deltaVar // arrow param; must only differ from deltaVar
	return []byte(fmt.Sprintf(
		`case"thinking_delta":{let %[1]s=%[2]s.event.delta;`+
			`if(%[1]s.thinking)%[3]s.onStreamingThinking?.(%[4]s=>({thinking:(%[4]s?.thinking??"")+%[1]s.thinking,`+liveThinkingPatchMarker+`}));`+
			`%[5]s?.({type:"thinking_progress",estimatedTokensDelta:%[1]s.estimated_tokens??%[6]s(%[1]s.thinking??"")});return}`,
		deltaVar, eventVar, handlersVar, prevVar, callbackVar, estimatorFn))
}

// --- launch-version patch -------------------------------------------------
//
// Rewrites the version string builder so the launch card version ends in "p",
// letting users confirm at a glance that the patched binary is running.

const launchVersionPatchMarker = `p${""}`

var launchVersionPatchAnchor = []byte("process.env.DEMO_VERSION??`${{ISSUES_EXPLAINER:")

var launchVersionSuffixRe = regexp.MustCompile(`\}\.VERSION\}(\$\{` + claudeIdent + `\(\)\})`)

const launchVersionScanLimit = 2048

func findLaunchVersionEdits(data []byte) ([]claudePatchEdit, bool, error) {
	anchor := bytes.Index(data, launchVersionPatchAnchor)
	if anchor < 0 {
		return nil, false, nil
	}
	if bytes.Index(data[anchor+len(launchVersionPatchAnchor):], launchVersionPatchAnchor) >= 0 {
		return nil, false, fmt.Errorf("Claude launch-version anchor matched more than once")
	}
	end := anchor + launchVersionScanLimit
	if end > len(data) {
		end = len(data)
	}
	m := launchVersionSuffixRe.FindSubmatchIndex(data[anchor:end])
	if m == nil {
		return nil, false, nil
	}
	offset, length := anchor+m[2], m[3]-m[2]
	replacement := []byte(launchVersionPatchMarker)
	if len(replacement) > length {
		return nil, false, fmt.Errorf("Claude launch-version replacement (%d bytes) exceeds target (%d bytes)", len(replacement), length)
	}
	return []claudePatchEdit{{offset: offset, length: length, replacement: replacement}}, true, nil
}

// --- patch application ------------------------------------------------

type claudePatchResult struct {
	Path       string
	Patched    bool
	Throughput bool
	Warnings   []string
}

// prepareClaudePatch returns a patched copy of Claude Code with every
// compatible feature enabled. Required features fail the operation; optional
// functional features report a warning and do not prevent the other patches
// from being used. The user's installed claude binary is never mutated.
func prepareClaudePatch(claudePath string) (claudePatchResult, error) {
	result := claudePatchResult{Path: claudePath}
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_DISABLE_CLAUDE_PATCH")) != "" {
		return result, nil
	}
	data, err := os.ReadFile(claudePath)
	if err != nil {
		return result, err
	}

	patchedData := append([]byte(nil), data...)
	for _, patch := range claudeBinaryPatches {
		if patch.disabled() {
			continue
		}
		if patch.applied(patchedData) {
			continue
		}
		edits, ok, err := patch.find(patchedData)
		if err != nil {
			if patch.required {
				return result, err
			}
			if patch.warnIfUnsupported {
				result.Warnings = append(result.Warnings, fmt.Sprintf("Claude %s patch failed; live token rate is unavailable: %v", patch.name, err))
			}
			continue
		}
		if !ok {
			if patch.required {
				return result, fmt.Errorf("required Claude %s patch found no target; this Claude Code build is not yet supported", patch.name)
			}
			if patch.warnIfUnsupported {
				result.Warnings = append(result.Warnings, fmt.Sprintf("Claude %s patch found no target; live token rate is unavailable for this Claude Code build", patch.name))
			}
			continue
		}
		for _, edit := range edits {
			if edit.offset < 0 || edit.length < len(edit.replacement) || edit.offset+edit.length > len(patchedData) {
				return result, fmt.Errorf("invalid Claude %s patch edit at offset %d", patch.name, edit.offset)
			}
			copy(patchedData[edit.offset:], edit.replacement)
			for i := edit.offset + len(edit.replacement); i < edit.offset+edit.length; i++ {
				patchedData[i] = ' '
			}
		}
	}
	if !requiredClaudePatchesApplied(patchedData) {
		return result, fmt.Errorf("required Claude patch verification failed")
	}
	result.Patched = true
	result.Throughput = claudePatchApplied("throughput-meter", patchedData)
	if bytes.Equal(patchedData, data) {
		return result, nil
	}

	sum := sha256.Sum256(data)
	target, err := claudePatchPath(claudePath, sum[:])
	if err != nil {
		return result, err
	}
	if existing, err := os.ReadFile(target); err == nil && cachedClaudePatchMatches(existing, patchedData) {
		result.Path = target
		result.Throughput = claudePatchApplied("throughput-meter", existing)
		return result, nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return result, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "claude-patched-*.tmp")
	if err != nil {
		return result, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(patchedData); err != nil {
		tmp.Close()
		return result, err
	}
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o755); err != nil {
			tmp.Close()
			return result, err
		}
	}
	if err := tmp.Close(); err != nil {
		return result, err
	}
	if _, err := os.Stat(target); err == nil {
		if err := os.Remove(target); err != nil {
			return result, err
		}
	}
	if err := os.Rename(tmpName, target); err != nil {
		return result, err
	}
	result.Path = target
	return result, nil
}

// prepareClaudeLiveThinkingPatch preserves the original internal call shape
// for focused tests and downstream packages that have not adopted the richer
// feature result yet.
func prepareClaudeLiveThinkingPatch(claudePath string) (path string, patched bool, err error) {
	result, err := prepareClaudePatch(claudePath)
	return result.Path, result.Patched, err
}

func claudePatchApplied(name string, data []byte) bool {
	for _, patch := range claudeBinaryPatches {
		if patch.name == name {
			return patch.applied(data)
		}
	}
	return false
}

func cachedClaudePatchMatches(existing, desired []byte) bool {
	if !requiredClaudePatchesApplied(existing) {
		return false
	}
	for _, patch := range claudeBinaryPatches {
		if patch.warnIfUnsupported && patch.applied(existing) != patch.applied(desired) {
			return false
		}
	}
	return true
}

func requiredClaudePatchesApplied(data []byte) bool {
	for _, patch := range claudeBinaryPatches {
		if patch.required && !patch.applied(data) {
			return false
		}
	}
	return true
}

func claudePatchPath(claudePath string, sum []byte) (string, error) {
	cfgPath, err := configPath()
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(claudePath)
	if ext == "" && runtime.GOOS == "windows" {
		ext = ".exe"
	}
	hash := hex.EncodeToString(sum)
	if len(hash) > 16 {
		hash = hash[:16]
	}
	name := fmt.Sprintf("claude-live-thinking-%s-%s-%s%s", runtime.GOOS, runtime.GOARCH, hash, ext)
	return filepath.Join(filepath.Dir(cfgPath), "claude-patches", name), nil
}
