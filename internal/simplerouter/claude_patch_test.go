package simplerouter

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeLiveThinkingCase mimics the minified thinking_delta case with identifier
// names that differ from any real build, proving the matcher is structural.
const fakeLiveThinkingCase = `case"thinking_delta":{let{delta:dl}=ev.event;if("estimated_tokens"in dl&&typeof dl.estimated_tokens==="number")c9?.({type:"thinking_progress",estimatedTokensDelta:dl.estimated_tokens});else if("thinking"in dl&&typeof dl.thinking==="string"&&dl.thinking.length>0)c9?.({type:"thinking_progress",estimatedTokensDelta:tk8(dl.thinking)});return}`

const fakeStreamFnPrefix = `function Xq2(ev,hd,ag){let{onSetStreamMode:s1,onApiMetrics:c9,onUpdateLength:u2}=hd;switch(ev.event.type){`

func fakePatchableClaudeBundle() []byte {
	out := []byte("prefix:")
	out = append(out, fakeStreamFnPrefix...)
	out = append(out, fakeLiveThinkingCase...)
	out = append(out, []byte(`}}:middle:`)...)
	out = append(out, launchVersionPatchAnchor...)
	out = append(out, []byte(`"report the issue at https://github.com/anthropics/claude-code/issues",PACKAGE_URL:"@anthropic-ai/claude-code",README_URL:"https://code.claude.com/docs/en/overview",VERSION:"2.1.205",FEEDBACK_CHANNEL:"https://github.com/anthropics/claude-code/issues"}.VERSION}`)...)
	out = append(out, []byte("${Q7z()}`")...)
	out = append(out, []byte(":suffix")...)
	return out
}

func TestPrepareClaudeLiveThinkingPatchCreatesPatchedCopy(t *testing.T) {
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	original := fakePatchableClaudeBundle()
	if err := os.WriteFile(src, original, 0o755); err != nil {
		t.Fatal(err)
	}

	got, patched, err := prepareClaudeLiveThinkingPatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !patched {
		t.Fatal("patched = false, want true")
	}
	if got == src {
		t.Fatalf("patched path = source path %q", got)
	}
	if !stringsHasPathPrefix(got, filepath.Join(home, configDirName, "claude-patches")) {
		t.Fatalf("patched path %q not under test config dir", got)
	}

	sourceAfter, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceAfter, original) {
		t.Fatal("source binary was modified")
	}
	patchedData, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(patchedData) != len(original) {
		t.Fatalf("patched length = %d, want %d", len(patchedData), len(original))
	}
	text := string(patchedData)
	// The rewritten case must use this build's identifier names: dl for the
	// delta, ev for the event, hd for the handlers object, c9 for the metrics
	// callback, and tk8 for the token estimator.
	wantCase := `case"thinking_delta":{let dl=ev.event.delta;` +
		`if(dl.thinking)hd.onStreamingThinking?.(_dl=>({thinking:(_dl?.thinking??"")+dl.thinking,isStreaming:!0}));` +
		`c9?.({type:"thinking_progress",estimatedTokensDelta:dl.estimated_tokens??tk8(dl.thinking??"")});return}`
	if !strings.Contains(text, wantCase) {
		t.Fatalf("patched binary missing rewritten thinking_delta case:\n%s", text)
	}
	if strings.Contains(text, `estimatedTokensDelta:tk8(dl.thinking)}`) {
		t.Fatal("patched binary still contains the original thinking_delta case")
	}
	if !strings.Contains(text, launchVersionPatchMarker) {
		t.Fatal("patched binary missing launch-version marker")
	}
	if strings.Contains(text, "${Q7z()}") {
		t.Fatal("patched binary still contains the launch-version target")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(got); err != nil {
			t.Fatal(err)
		} else if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("patched file is not executable: %v", info.Mode())
		}
	}

	got2, patched2, err := prepareClaudeLiveThinkingPatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !patched2 || got2 != got {
		t.Fatalf("second patch = (%q, %v), want (%q, true)", got2, patched2, got)
	}
}

func TestPrepareClaudeLiveThinkingPatchRewritesIncompleteCachedCopy(t *testing.T) {
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	original := fakePatchableClaudeBundle()
	if err := os.WriteFile(src, original, 0o755); err != nil {
		t.Fatal(err)
	}

	got, patched, err := prepareClaudeLiveThinkingPatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !patched {
		t.Fatal("patched = false, want true")
	}

	// Simulate a cached copy from an older simplerouter that only applied the
	// live-thinking patch: it must be rebuilt with all patches.
	stale := append([]byte(nil), original...)
	edits, ok, err := findLiveThinkingEdits(stale)
	if err != nil || !ok {
		t.Fatalf("findLiveThinkingEdits = (%v, %v)", ok, err)
	}
	for _, edit := range edits {
		copy(stale[edit.offset:], edit.replacement)
		for i := edit.offset + len(edit.replacement); i < edit.offset+edit.length; i++ {
			stale[i] = ' '
		}
	}
	if err := os.WriteFile(got, stale, 0o755); err != nil {
		t.Fatal(err)
	}

	got2, patched2, err := prepareClaudeLiveThinkingPatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !patched2 || got2 != got {
		t.Fatalf("second patch = (%q, %v), want (%q, true)", got2, patched2, got)
	}
	patchedData, err := os.ReadFile(got2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patchedData, []byte(launchVersionPatchMarker)) {
		t.Fatal("cached patch was not rewritten with launch-version replacement")
	}
}

func TestPrepareClaudeLiveThinkingPatchFallsBackForUnsupportedBinary(t *testing.T) {
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("not a supported Claude Code bundle"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, patched, err := prepareClaudeLiveThinkingPatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if patched || got != src {
		t.Fatalf("patch = (%q, %v), want (%q, false)", got, patched, src)
	}
}

func TestFindLiveThinkingEditsRequiresHandlersDestructure(t *testing.T) {
	// The same case body without an enclosing onSetStreamMode destructure
	// (e.g. the SDK copy of the dispatcher) must not be patched.
	bundle := append([]byte("no handlers here:"), fakeLiveThinkingCase...)
	edits, ok, err := findLiveThinkingEdits(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(edits) != 0 {
		t.Fatalf("edits = %v, ok = %v; want none", edits, ok)
	}
}

func TestFindLiveThinkingEditsPatchesEveryQualifyingCopy(t *testing.T) {
	one := append([]byte(fakeStreamFnPrefix), fakeLiveThinkingCase...)
	two := strings.ReplaceAll(string(one), "dl", "qq")
	two = strings.ReplaceAll(two, "hd", "zz")
	bundle := append(one, two...)
	edits, ok, err := findLiveThinkingEdits(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(edits) != 2 {
		t.Fatalf("edits = %d, ok = %v; want 2 qualifying copies patched", len(edits), ok)
	}
	if !bytes.Contains(edits[1].replacement, []byte("zz.onStreamingThinking")) {
		t.Fatalf("second copy resolved wrong handlers var: %s", edits[1].replacement)
	}
}

// TestClaudePatchesMatchInstalledClaude is a canary: when a real Claude Code
// binary is installed, every patch must either already be applied or locate
// its target in the current build. A failure means a Claude update changed
// the code shape and the structural matcher needs updating.
func TestClaudePatchesMatchInstalledClaude(t *testing.T) {
	claudePath, err := findClaude()
	if err != nil {
		t.Skip("claude binary not installed")
	}
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Skipf("cannot read claude binary: %v", err)
	}
	for _, patch := range claudeBinaryPatches {
		if patch.applied(data) {
			continue
		}
		edits, ok, err := patch.find(data)
		if err != nil {
			t.Fatalf("%s: %v", patch.name, err)
		}
		if !ok {
			t.Fatalf("%s patch found no target in installed Claude Code (%s)", patch.name, claudePath)
		}
		for _, edit := range edits {
			if len(edit.replacement) > edit.length {
				t.Fatalf("%s replacement (%d bytes) exceeds target (%d bytes)", patch.name, len(edit.replacement), edit.length)
			}
		}
	}
}

func stringsHasPathPrefix(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	return err == nil && rel != "." && rel != ".." && !bytes.HasPrefix([]byte(rel), []byte(".."+string(filepath.Separator)))
}
