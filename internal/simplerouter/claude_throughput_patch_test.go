package simplerouter

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fakeAPIMetricsFunction = `function Nxp({entries:e,responseLength:t,event:r}){if(r.type==="start")return e.push({id:r.id,ttftMs:r.ttftMs,firstTokenTime:Date.now(),lastTokenTime:Date.now(),responseLengthBaseline:t,endResponseLength:t}),t;let n=r.id!=null?e.find((o)=>o.id===r.id):e.findLast((o)=>o.id==null);if(!n)return t;if(r.type==="content_block_start")return n.thinkingTokenEstimate=0,n.thinkingBlockBaseline=t,n.sawEstimatedTokensThisBlock=!1,t;if(r.type==="thinking_progress"){if(n.sawEstimatedTokensThisBlock=!0,n.thinkingTokenEstimate=(n.thinkingTokenEstimate??0)+r.estimatedTokensDelta,n.outputTokens==null&&r.id==null){let o=n.thinkingBlockBaseline??n.responseLengthBaseline;return Math.max(t,o+n.thinkingTokenEstimate*4)}return t}if(r.type==="thinking_signature"){if(r.chars>0&&n.outputTokens==null){if(n.lastTokenTime=Date.now(),n.sawEstimatedTokensThisBlock){n.thinkingTokenEstimate=Math.max(n.thinkingTokenEstimate??0,Math.ceil(r.chars/4));let i=n.thinkingBlockBaseline??n.responseLengthBaseline,s=Math.max(t,i+n.thinkingTokenEstimate*4);return n.endResponseLength=s,s}let o=t+r.chars;return n.endResponseLength=o,o}return t}if(n.outputTokens=r.outputTokens,n.lastTokenTime=Date.now(),r.id==null)return Math.max(t,n.responseLengthBaseline+r.outputTokens*4);return t}`

const fakeThroughputSpinner = `function XSpinner(){let R=Date.now(),dummy=0;let ve=t?P:ae.current,he=ra(D),De=Ft(he),Ae=de,Ce=_d(Ae),$e=` + "`${je.arrowDown} ${Ce} tokens`" + `,ge=Ft($e),Oe=N.kind==="thinking"?Otb(N.thinkingMs):"thinking",Be;switch(N.kind){case"tool-running":Be=` + "`running tool for ${ra(N.toolMs)}`" + `;break;case"tool-done":Be=` + "`ran tool for ${ra(N.toolMs)}`" + `;break;case"thinking":Be=` + "`${Oe}${g}`" + `;break;case"thought-for":Be=` + "`thought for ${Math.max(1,Math.round(N.thoughtMs/1000))}s`" + `;break;case"none":Be=null;break}let Le=Be?Ft(Be):0,ze=0,_r=[...!I&&gr?[Np.jsxs(H,{children:[Np.jsx(bxp,{}),Np.jsxs(h,{dimColor:!0,children:[Ce," tokens"]})]})]:[]];return _r}`

const fakeThroughputController = `tq=Hr.useCallback((_t)=>{if(_t.op==="reset")OLe();else vO(_t.delta)},[vO,OLe]),VF=Hr.useCallback((_t)=>{if(_t.type==="start"&&_t.messageId!=null)Rne.current=_t.messageId;let dr=_t.type==="thinking_signature"&&$x()?EO.current.findLast(($r)=>$r.id==null)?.thinkingTokenEstimate??0:void 0;if(GF.current=Nxp({entries:EO.current,responseLength:GF.current,event:_t}),_t.type==="thinking_progress"&&$x()){let $r=EO.current.findLast((Wn)=>Wn.id==null);if($r?.thinkingTokenEstimate!=null)lA({type:"system",subtype:"thinking_tokens",estimated_tokens:$r.thinkingTokenEstimate,estimated_tokens_delta:_t.estimatedTokensDelta})}else if(dr!==void 0){let $r=EO.current.findLast((Wn)=>Wn.id==null)?.thinkingTokenEstimate;if($r!=null&&$r>dr)lA({type:"system",subtype:"thinking_tokens",estimated_tokens:$r,estimated_tokens_delta:$r-dr})}},[]),Mx=Hr.useMemo(cYp,[]),Lx=Hr.useMemo(()=>aYp({scheduleTimeout:W.setTimeout,onFlush:Mx.setRaw}),[W,Mx]);Hr.useEffect(()=>()=>Lx.dispose(),[Lx]);let Bge=!(qe((_t)=>_t.settings.prefersReducedMotion)??!1)&&!y2u(),_K=Hr.useCallback((_t)=>{if(!Bge){if(_t(Lx.peek())===null)Lx.clear();return}Lx.apply(_t)},[Bge,Lx]),uM=Hr.useCallback(()=>{GF.current=0,EO.current=[],Rne.current=null},[]),Dne=_9f({setMessages:wd,recordApiMetricsEvent:VF,onTurnEnd:uM}),wd((e)=>e.concat(ml("later","info")))`

func fakeThroughputBundle() []byte {
	return []byte(`function ml(e,t,r,n){return{type:"system",subtype:"informational",content:e,isMeta:!1,timestamp:new Date().toISOString(),uuid:Jz.randomUUID(),toolUseID:r,level:t,...n&&{preventContinuation:n}}}` + fakeThroughputSpinner + fakeAPIMetricsFunction + fakeThroughputController)
}

func TestFindThroughputEditsRewritesAllInMemoryHooks(t *testing.T) {
	original := fakeThroughputBundle()
	if _, metricsFn, ok, err := findThroughputStateEdit(original); err != nil || !ok {
		t.Fatalf("findThroughputStateEdit = (%q, %v, %v)", metricsFn, ok, err)
	}
	if _, ok, err := findThroughputSpinnerEdit(original); err != nil || !ok {
		matches := spinnerMetricRe.FindAllSubmatchIndex(original, -1)
		detail := ""
		if len(matches) == 1 {
			groups := namedSubmatches(spinnerMetricRe, original, matches[0])
			start, end, enclosed := enclosingJSFunction(original, matches[0][0])
			innerRe := regexp.MustCompile(`(` + claudeIdent + `)\.jsxs\((` + claudeIdent + `),\{dimColor:!0,children:\[` + regexp.QuoteMeta(groups["tokenText"]) + `," tokens"\]\}\)`)
			inners := innerRe.FindAllSubmatchIndex(original[matches[0][1]:end], -1)
			detail = fmt.Sprintf(" consistent=%v enclosed=%v range=%d:%d before=%q now=%v inner=%d groups=%v", spinnerGroupsConsistent(groups), enclosed, start, end, original[start:matches[0][0]], regexp.MustCompile(`(?:let |,)(`+claudeIdent+`)=Date\.now\(\),`).FindAllSubmatch(original[start:matches[0][0]], -1), len(inners), groups)
		}
		t.Fatalf("findThroughputSpinnerEdit = (%v, %v), metric matches=%d%s", ok, err, len(matches), detail)
	}
	if _, ok, err := findThroughputControllerEdit(original, "Nxp"); err != nil || !ok {
		t.Fatalf("findThroughputControllerEdit = (%v, %v)", ok, err)
	}
	edits, ok, err := findThroughputEdits(original)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(edits) != 3 {
		t.Fatalf("findThroughputEdits = (%d edits, %v), want (3, true)", len(edits), ok)
	}
	patched := applyClaudePatchEditsForTest(t, original, edits)
	if len(patched) != len(original) {
		t.Fatalf("patched length = %d, want %d", len(patched), len(original))
	}
	for _, marker := range []string{throughputStatePatchMarker, throughputSpinnerPatchMarker, throughputFinalPatchMarker} {
		if !bytes.Contains(patched, []byte(marker)) {
			t.Errorf("patched bundle missing marker %q", marker)
		}
	}
	text := string(patched)
	if strings.Contains(text, `children:[Ce," tokens"]`) {
		t.Fatal("spinner still renders the old token-count label")
	}
	if !strings.Contains(text, `s.t.toLocaleString()} output tokens \xB7 ${(s.t*1e3/s.m).toFixed(1)} tok/s`) {
		t.Fatal("turn-end line does not use the exact aggregate token count and generation duration")
	}
	if !strings.Contains(text, `s&&!s.d&&`) {
		t.Fatal("turn-end aggregate is not guarded against duplicate emission")
	}
	if !strings.Contains(text, `tq=Hr.useCallback((_t)=>{if(_t.op==="reset")OLe();else vO(_t.delta)},[vO,OLe]),VF=Hr.useCallback(`) {
		t.Fatal("controller rewrite removed or renamed an adjacent callback binding")
	}
	if !strings.Contains(text, `recordApiMetricsEvent:VF`) {
		t.Fatal("controller rewrite broke the metrics callback reference")
	}
}

func TestPrepareClaudePatchReportsThroughputFeature(t *testing.T) {
	t.Setenv("SIMPLEROUTER_DISABLE_CLAUDE_PATCH", "")
	t.Setenv("SIMPLEROUTER_DISABLE_TOKEN_RATE", "")
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	original := append(fakePatchableClaudeBundle(), fakeThroughputBundle()...)
	if err := os.WriteFile(src, original, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Patched || !result.Throughput {
		t.Fatalf("patch result = %+v, want live-thinking and throughput", result)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", result.Warnings)
	}
	patched, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !claudePatchApplied("throughput-meter", patched) {
		t.Fatal("prepared binary does not contain the complete throughput patch")
	}
	if sourceAfter, err := os.ReadFile(src); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(sourceAfter, original) {
		t.Fatal("source binary was modified")
	}
}

func TestPrepareClaudePatchCanDisableOnlyThroughput(t *testing.T) {
	t.Setenv("SIMPLEROUTER_DISABLE_CLAUDE_PATCH", "")
	t.Setenv("SIMPLEROUTER_DISABLE_TOKEN_RATE", "1")
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, append(fakePatchableClaudeBundle(), fakeThroughputBundle()...), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Patched || result.Throughput {
		t.Fatalf("patch result = %+v, want only required patches", result)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for an explicitly disabled feature", result.Warnings)
	}
	patched, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if claudePatchApplied("throughput-meter", patched) {
		t.Fatal("throughput patch was applied despite SIMPLEROUTER_DISABLE_TOKEN_RATE")
	}
}

func TestPrepareClaudePatchRefreshesCachedFeatureSet(t *testing.T) {
	t.Setenv("SIMPLEROUTER_DISABLE_CLAUDE_PATCH", "")
	t.Setenv("SIMPLEROUTER_DISABLE_TOKEN_RATE", "1")
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, append(fakePatchableClaudeBundle(), fakeThroughputBundle()...), 0o755); err != nil {
		t.Fatal(err)
	}

	withoutThroughput, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if withoutThroughput.Throughput {
		t.Fatal("disabled throughput unexpectedly appeared in the cached patch")
	}

	t.Setenv("SIMPLEROUTER_DISABLE_TOKEN_RATE", "")
	withThroughput, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !withThroughput.Throughput {
		t.Fatalf("cached patch was not refreshed when throughput was enabled: %+v", withThroughput)
	}
	if withThroughput.Path != withoutThroughput.Path {
		t.Fatalf("cache path changed for the same source: %q != %q", withThroughput.Path, withoutThroughput.Path)
	}
	patched, err := os.ReadFile(withThroughput.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !claudePatchApplied("throughput-meter", patched) {
		t.Fatal("refreshed cached binary is missing throughput markers")
	}
}

func TestPrepareClaudePatchRefreshesStaleImplementation(t *testing.T) {
	t.Setenv("SIMPLEROUTER_DISABLE_CLAUDE_PATCH", "")
	t.Setenv("SIMPLEROUTER_DISABLE_TOKEN_RATE", "")
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, append(fakePatchableClaudeBundle(), fakeThroughputBundle()...), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	stale := append([]byte(nil), desired...)
	marker := bytes.Index(stale, []byte(throughputFinalPatchMarker))
	if marker < 1 {
		t.Fatal("prepared patch is missing throughput marker")
	}
	stale[marker-1] ^= 1
	if err := os.WriteFile(first.Path, stale, 0o755); err != nil {
		t.Fatal(err)
	}

	refreshed, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(refreshed.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, desired) {
		t.Fatal("stale cached patch was not replaced with the current implementation")
	}
}

func TestPrepareClaudePatchIsolatesUnsupportedThroughput(t *testing.T) {
	t.Setenv("SIMPLEROUTER_DISABLE_CLAUDE_PATCH", "")
	t.Setenv("SIMPLEROUTER_DISABLE_TOKEN_RATE", "")
	home := withTestHome(t)
	src := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, fakePatchableClaudeBundle(), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := prepareClaudePatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Patched || result.Throughput {
		t.Fatalf("patch result = %+v, want live-thinking fallback without throughput", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "live token rate is unavailable") {
		t.Fatalf("warnings = %v, want one targeted throughput warning", result.Warnings)
	}
}

func applyClaudePatchEditsForTest(t *testing.T, original []byte, edits []claudePatchEdit) []byte {
	t.Helper()
	patched := append([]byte(nil), original...)
	for _, edit := range edits {
		if len(edit.replacement) > edit.length {
			t.Fatalf("replacement (%d bytes) exceeds target (%d bytes)", len(edit.replacement), edit.length)
		}
		copy(patched[edit.offset:], edit.replacement)
		for index := edit.offset + len(edit.replacement); index < edit.offset+edit.length; index++ {
			patched[index] = ' '
		}
	}
	return patched
}
