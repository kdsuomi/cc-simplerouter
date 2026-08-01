package simplerouter

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Claude already records first/last-token timestamps, estimated response
// length, and authoritative output_tokens for every API call. The throughput
// patch exposes that existing state to the spinner and emits one finalized
// informational line at turn end. It does not inspect transcript files or
// duplicate provider-specific stream accounting.
const (
	throughputStatePatchMarker   = `globalThis.__sr={e:`
	throughputSpinnerPatchMarker = `globalThis.__sr??{}`
	throughputFinalPatchMarker   = `output tokens \xB7`
)

var (
	apiMetricsFunctionHeaderRe = regexp.MustCompile(
		`function (` + claudeIdent + `)\(\{entries:(` + claudeIdent + `),responseLength:(` + claudeIdent + `),event:(` + claudeIdent + `)\}\)\{`)
	jsIdentifierRe = regexp.MustCompile(claudeIdent)

	spinnerMetricRe = regexp.MustCompile(
		`let (?P<ve>` + claudeIdent + `)=(?P<reduced>` + claudeIdent + `)\?(?P<thinkingProgress>` + claudeIdent + `):(?P<smoothedThinking>` + claudeIdent + `)\.current,` +
			`(?P<elapsedText>` + claudeIdent + `)=(?P<formatDuration>` + claudeIdent + `)\((?P<elapsedMs>` + claudeIdent + `)\),` +
			`(?P<elapsedWidth>` + claudeIdent + `)=(?P<measure>` + claudeIdent + `)\((?P<elapsedText2>` + claudeIdent + `)\),` +
			`(?P<tokenCount>` + claudeIdent + `)=(?P<smoothedTokens>` + claudeIdent + `),` +
			`(?P<tokenText>` + claudeIdent + `)=_d\((?P<tokenCount2>` + claudeIdent + `)\),` +
			"(?P<oldLabel>" + claudeIdent + ")=`\\$\\{(?P<symbols>" + claudeIdent + ")\\.arrowDown\\} \\$\\{(?P<tokenText2>" + claudeIdent + ")\\} tokens`," +
			`(?P<tokenWidth>` + claudeIdent + `)=(?P<measure2>` + claudeIdent + `)\((?P<oldLabel2>` + claudeIdent + `)\),` +
			`(?P<thinkingText>` + claudeIdent + `)=(?P<status>` + claudeIdent + `)\.kind==="thinking"\?(?P<thinkingLabel>` + claudeIdent + `)\((?P<status2>` + claudeIdent + `)\.thinkingMs\):"thinking",` +
			`(?P<detail>` + claudeIdent + `);switch\((?P<status3>` + claudeIdent + `)\.kind\)\{` +
			"case\"tool-running\":(?P<detail2>" + claudeIdent + ")=`running tool for \\$\\{(?P<formatDuration2>" + claudeIdent + ")\\((?P<status4>" + claudeIdent + ")\\.toolMs\\)\\}`;break;" +
			"case\"tool-done\":(?P<detail3>" + claudeIdent + ")=`ran tool for \\$\\{(?P<formatDuration3>" + claudeIdent + ")\\((?P<status5>" + claudeIdent + ")\\.toolMs\\)\\}`;break;" +
			"case\"thinking\":(?P<detail4>" + claudeIdent + ")=`\\$\\{(?P<thinkingText2>" + claudeIdent + ")\\}\\$\\{(?P<effortSuffix>" + claudeIdent + ")\\}`;break;" +
			"case\"thought-for\":(?P<detail5>" + claudeIdent + ")=`thought for \\$\\{Math\\.max\\(1,Math\\.round\\((?P<status6>" + claudeIdent + ")\\.thoughtMs/1000\\)\\)\\}s`;break;" +
			`case"none":(?P<detail6>` + claudeIdent + `)=null;break\}` +
			`let (?P<detailWidth>` + claudeIdent + `)=(?P<detail7>` + claudeIdent + `)\?(?P<measure3>` + claudeIdent + `)\((?P<detail8>` + claudeIdent + `)\):0,`)

	controllerCallbackHeaderRe = regexp.MustCompile(
		`(` + claudeIdent + `)=(` + claudeIdent + `)\.useCallback\(\((` + claudeIdent + `)\)=>\{`)
	controllerTailRe = regexp.MustCompile(
		`let (?P<motion>` + claudeIdent + `)=!\((?P<select>` + claudeIdent + `)\(\((?P<pref1>` + claudeIdent + `)\)=>(?P<pref2>` + claudeIdent + `)\.settings\.prefersReducedMotion\)\?\?!1\)&&!(?P<disableMotion>` + claudeIdent + `)\(\),` +
			`(?P<streamCallback>` + claudeIdent + `)=(?P<react>` + claudeIdent + `)\.useCallback\(\((?P<update1>` + claudeIdent + `)\)=>\{if\(!(?P<motion2>` + claudeIdent + `)\)\{if\((?P<update2>` + claudeIdent + `)\((?P<buffer1>` + claudeIdent + `)\.peek\(\)\)===null\)(?P<buffer2>` + claudeIdent + `)\.clear\(\);return\}(?P<buffer3>` + claudeIdent + `)\.apply\((?P<update3>` + claudeIdent + `)\)\},\[(?P<motion3>` + claudeIdent + `),(?P<buffer4>` + claudeIdent + `)\]\),` +
			`(?P<turnEnd>` + claudeIdent + `)=(?P<react2>` + claudeIdent + `)\.useCallback\(\(\)=>\{(?P<responseRef>` + claudeIdent + `)\.current=0,(?P<entriesRef>` + claudeIdent + `)\.current=\[\],(?P<messageRef>` + claudeIdent + `)\.current=null\},\[\]\),`)
	informationalMessageFactoryRe = regexp.MustCompile(
		`function (` + claudeIdent + `)\([^)]*\)\{return\{type:"system",subtype:"informational",content:`)
)

func findThroughputEdits(data []byte) ([]claudePatchEdit, bool, error) {
	stateEdit, metricsFn, ok, err := findThroughputStateEdit(data)
	if err != nil || !ok {
		return nil, false, err
	}
	spinnerEdit, ok, err := findThroughputSpinnerEdit(data)
	if err != nil || !ok {
		return nil, false, err
	}
	controllerEdit, ok, err := findThroughputControllerEdit(data, metricsFn)
	if err != nil || !ok {
		return nil, false, err
	}
	return []claudePatchEdit{stateEdit, spinnerEdit, controllerEdit}, true, nil
}

func findThroughputStateEdit(data []byte) (claudePatchEdit, string, bool, error) {
	matches := apiMetricsFunctionHeaderRe.FindAllSubmatchIndex(data, -1)
	var found []struct {
		edit claudePatchEdit
		fn   string
	}
	for _, match := range matches {
		openBrace := match[1] - 1
		end, ok := findJSBlockEnd(data, openBrace)
		if !ok {
			continue
		}
		original := data[match[0]:end]
		if !bytes.Contains(original, []byte("thinkingTokenEstimate")) ||
			!bytes.Contains(original, []byte("sawEstimatedTokensThisBlock")) ||
			!bytes.Contains(original, []byte("responseLengthBaseline")) ||
			!bytes.Contains(original, []byte("n.outputTokens=r.outputTokens")) && !bytes.Contains(original, []byte("outputTokens")) {
			continue
		}
		fn := submatchString(data, match, 1)
		entries := submatchString(data, match, 2)
		responseLength := submatchString(data, match, 3)
		event := submatchString(data, match, 4)
		replacement, err := buildThroughputStateReplacement(fn, entries, responseLength, event)
		if err != nil {
			return claudePatchEdit{}, "", false, err
		}
		if len(replacement) > len(original) {
			return claudePatchEdit{}, "", false, fmt.Errorf("Claude throughput state replacement (%d bytes) exceeds target (%d bytes)", len(replacement), len(original))
		}
		found = append(found, struct {
			edit claudePatchEdit
			fn   string
		}{claudePatchEdit{offset: match[0], length: len(original), replacement: replacement}, fn})
	}
	if len(found) == 0 {
		return claudePatchEdit{}, "", false, nil
	}
	if len(found) != 1 {
		return claudePatchEdit{}, "", false, fmt.Errorf("Claude throughput metrics function matched %d targets", len(found))
	}
	return found[0].edit, found[0].fn, true, nil
}

func buildThroughputStateReplacement(fn, entries, responseLength, event string) ([]byte, error) {
	locals, ok := freshIdentifiers(5, fn, entries, responseLength, event)
	if !ok {
		return nil, fmt.Errorf("could not allocate identifiers for Claude throughput state patch")
	}
	entry, estimateKey, baselineKey, sawKey, current := locals[0], locals[1], locals[2], locals[3], locals[4]
	template := `function {{FN}}({entries:{{ENTRIES}},responseLength:{{LENGTH}},event:{{EVENT}}}){if({{EVENT}}.type==="start"){if({{EVENT}}.id==null){let {{CURRENT}}=globalThis.__sr;{{CURRENT}}?.e==={{ENTRIES}}&&!{{CURRENT}}.d||({{CURRENT}}=globalThis.__sr={e:{{ENTRIES}},t:0,m:0}),{{CURRENT}}.q={{CURRENT}}.t-{{LENGTH}}/4,{{CURRENT}}.p={{CURRENT}}.m-Date.now(),{{CURRENT}}.n=1}return {{ENTRIES}}.push({id:{{EVENT}}.id,ttftMs:{{EVENT}}.ttftMs,firstTokenTime:Date.now(),lastTokenTime:Date.now(),responseLengthBaseline:{{LENGTH}},endResponseLength:{{LENGTH}}}),{{LENGTH}}}let {{ENTRY}}={{EVENT}}.id!=null?{{ENTRIES}}.find({{CURRENT}}=>{{CURRENT}}.id==={{EVENT}}.id):{{ENTRIES}}.findLast({{CURRENT}}=>{{CURRENT}}.id==null);if(!{{ENTRY}})return {{LENGTH}};let {{ESTIMATE}}="thinkingTokenEstimate",{{BASELINE}}="thinkingBlockBaseline",{{SAW}}="sawEstimatedTokensThisBlock";if({{EVENT}}.type==="content_block_start"){{ENTRY}}[{{ESTIMATE}}]=0,{{ENTRY}}[{{BASELINE}}]={{LENGTH}},{{ENTRY}}[{{SAW}}]=!1;else if({{EVENT}}.type==="thinking_progress"){{ENTRY}}[{{SAW}}]=!0,{{ENTRY}}[{{ESTIMATE}}]=({{ENTRY}}[{{ESTIMATE}}]??0)+{{EVENT}}.estimatedTokensDelta,{{ENTRY}}.outputTokens==null&&{{EVENT}}.id==null&&({{LENGTH}}=Math.max({{LENGTH}},({{ENTRY}}[{{BASELINE}}]??{{ENTRY}}.responseLengthBaseline)+{{ENTRY}}[{{ESTIMATE}}]*4));else if({{EVENT}}.type==="thinking_signature"){if({{EVENT}}.chars>0&&{{ENTRY}}.outputTokens==null){{ENTRY}}.lastTokenTime=Date.now(),{{ENTRY}}[{{SAW}}]?({{ENTRY}}[{{ESTIMATE}}]=Math.max({{ENTRY}}[{{ESTIMATE}}]??0,Math.ceil({{EVENT}}.chars/4)),{{LENGTH}}=Math.max({{LENGTH}},({{ENTRY}}[{{BASELINE}}]??{{ENTRY}}.responseLengthBaseline)+{{ENTRY}}[{{ESTIMATE}}]*4)):{{LENGTH}}+={{EVENT}}.chars,{{ENTRY}}.endResponseLength={{LENGTH}}}else{{ENTRY}}.outputTokens={{EVENT}}.outputTokens,{{ENTRY}}.lastTokenTime=Date.now(),{{EVENT}}.id==null&&(globalThis.__sr&&(globalThis.__sr.t+={{EVENT}}.outputTokens??0,globalThis.__sr.m+=Math.max(1,{{ENTRY}}.lastTokenTime-{{ENTRY}}.firstTokenTime),globalThis.__sr.n=0),{{LENGTH}}=Math.max({{LENGTH}},{{ENTRY}}.responseLengthBaseline+{{EVENT}}.outputTokens*4));return {{LENGTH}}}`
	replacement := strings.NewReplacer(
		"{{FN}}", fn,
		"{{ENTRIES}}", entries,
		"{{LENGTH}}", responseLength,
		"{{EVENT}}", event,
		"{{ENTRY}}", entry,
		"{{ESTIMATE}}", estimateKey,
		"{{BASELINE}}", baselineKey,
		"{{SAW}}", sawKey,
		"{{CURRENT}}", current,
	).Replace(template)
	return []byte(replacement), nil
}

func findThroughputSpinnerEdit(data []byte) (claudePatchEdit, bool, error) {
	matches := spinnerMetricRe.FindAllSubmatchIndex(data, -1)
	var edits []claudePatchEdit
	for _, match := range matches {
		groups := namedSubmatches(spinnerMetricRe, data, match)
		if !spinnerGroupsConsistent(groups) {
			continue
		}
		functionStart, functionEnd, ok := enclosingJSFunction(data, match[0])
		if !ok {
			continue
		}
		beforeMetric := data[functionStart:match[0]]
		nowMatches := regexp.MustCompile(`(?:let |,)(`+claudeIdent+`)=Date\.now\(\),`).FindAllSubmatch(beforeMetric, -1)
		if len(nowMatches) == 0 {
			continue
		}
		now := string(nowMatches[len(nowMatches)-1][1])
		locals, ok := freshIdentifiersInFunction(data[functionStart:functionEnd], 2)
		if !ok {
			return claudePatchEdit{}, false, fmt.Errorf("could not allocate identifiers for Claude throughput spinner patch")
		}
		state, rate := locals[0], locals[1]
		prefix := buildThroughputSpinnerPrefix(groups, now, state, rate)

		innerRe := regexp.MustCompile(`(` + claudeIdent + `)\.jsxs\((` + claudeIdent + `),\{dimColor:!0,children:\[` + regexp.QuoteMeta(groups["tokenText"]) + `," tokens"\]\}\)`)
		innerMatches := innerRe.FindAllSubmatchIndex(data[match[1]:functionEnd], -1)
		if len(innerMatches) != 1 {
			continue
		}
		inner := innerMatches[0]
		innerStart := match[1] + inner[0]
		innerEnd := match[1] + inner[1]
		gap := data[match[1]:innerStart]
		if identifierAppears(gap, groups["oldLabel"]) {
			continue
		}
		renderer := submatchString(data[match[1]:functionEnd], inner, 1)
		textComponent := submatchString(data[match[1]:functionEnd], inner, 2)
		newInner := []byte(fmt.Sprintf(`%s.jsx(%s,{dimColor:!0,children:%s})`, renderer, textComponent, groups["tokenText"]))
		replacement := make([]byte, 0, len(prefix)+len(gap)+len(newInner))
		replacement = append(replacement, prefix...)
		replacement = append(replacement, gap...)
		replacement = append(replacement, newInner...)
		length := innerEnd - match[0]
		if len(replacement) > length {
			return claudePatchEdit{}, false, fmt.Errorf("Claude throughput spinner replacement (%d bytes) exceeds target (%d bytes)", len(replacement), length)
		}
		edits = append(edits, claudePatchEdit{offset: match[0], length: length, replacement: replacement})
	}
	if len(edits) == 0 {
		return claudePatchEdit{}, false, nil
	}
	if len(edits) != 1 {
		return claudePatchEdit{}, false, fmt.Errorf("Claude throughput spinner matched %d targets", len(edits))
	}
	return edits[0], true, nil
}

func buildThroughputSpinnerPrefix(g map[string]string, now, state, rate string) []byte {
	template := "let {{VE}}={{REDUCED}}?{{THINKING_PROGRESS}}:{{SMOOTHED_THINKING}}.current,{{ELAPSED_TEXT}}={{FORMAT_DURATION}}({{ELAPSED_MS}}),{{ELAPSED_WIDTH}}={{MEASURE}}({{ELAPSED_TEXT}}),{{TOKEN_COUNT}}={{SMOOTHED_TOKENS}},{{STATE}}=globalThis.__sr??{},{{RATE}}={{STATE}}.n?({{TOKEN_COUNT}}+{{STATE}}.q)*1e3/({{NOW}}+{{STATE}}.p):{{STATE}}.t*1e3/{{STATE}}.m,{{TOKEN_TEXT}}={{RATE}}?`${{{RATE}}.toFixed(1)} tok/s`:`${_d({{TOKEN_COUNT}})} tokens`,{{TOKEN_WIDTH}}={{MEASURE}}({{TOKEN_TEXT}})+2,{{THINKING_TEXT}}={{STATUS}}.kind===\"thinking\"?{{THINKING_LABEL}}({{STATUS}}.thinkingMs):\"thinking\",{{DETAIL}}={{STATUS}}.kind===\"tool-running\"?`running tool for ${{{FORMAT_DURATION}}({{STATUS}}.toolMs)}`:{{STATUS}}.kind===\"tool-done\"?`ran tool for ${{{FORMAT_DURATION}}({{STATUS}}.toolMs)}`:{{STATUS}}.kind===\"thinking\"?`${{{THINKING_TEXT}}}${{{EFFORT_SUFFIX}}}`:{{STATUS}}.kind===\"thought-for\"?`thought for ${Math.max(1,Math.round({{STATUS}}.thoughtMs/1000))}s`:null,{{DETAIL_WIDTH}}={{DETAIL}}?{{MEASURE}}({{DETAIL}}):0,"
	return []byte(strings.NewReplacer(
		"{{VE}}", g["ve"],
		"{{REDUCED}}", g["reduced"],
		"{{THINKING_PROGRESS}}", g["thinkingProgress"],
		"{{SMOOTHED_THINKING}}", g["smoothedThinking"],
		"{{ELAPSED_TEXT}}", g["elapsedText"],
		"{{FORMAT_DURATION}}", g["formatDuration"],
		"{{ELAPSED_MS}}", g["elapsedMs"],
		"{{ELAPSED_WIDTH}}", g["elapsedWidth"],
		"{{MEASURE}}", g["measure"],
		"{{TOKEN_COUNT}}", g["tokenCount"],
		"{{SMOOTHED_TOKENS}}", g["smoothedTokens"],
		"{{TOKEN_TEXT}}", g["tokenText"],
		"{{TOKEN_WIDTH}}", g["tokenWidth"],
		"{{THINKING_TEXT}}", g["thinkingText"],
		"{{STATUS}}", g["status"],
		"{{THINKING_LABEL}}", g["thinkingLabel"],
		"{{EFFORT_SUFFIX}}", g["effortSuffix"],
		"{{DETAIL}}", g["detail"],
		"{{DETAIL_WIDTH}}", g["detailWidth"],
		"{{NOW}}", now,
		"{{STATE}}", state,
		"{{RATE}}", rate,
	).Replace(template))
}

func spinnerGroupsConsistent(g map[string]string) bool {
	checks := [][2]string{
		{"elapsedText", "elapsedText2"}, {"measure", "measure2"}, {"measure", "measure3"},
		{"tokenCount", "tokenCount2"}, {"tokenText", "tokenText2"}, {"oldLabel", "oldLabel2"},
		{"formatDuration", "formatDuration2"}, {"formatDuration", "formatDuration3"},
		{"thinkingText", "thinkingText2"}, {"status", "status2"}, {"status", "status3"},
		{"status", "status4"}, {"status", "status5"}, {"status", "status6"},
		{"detail", "detail2"}, {"detail", "detail3"}, {"detail", "detail4"},
		{"detail", "detail5"}, {"detail", "detail6"}, {"detail", "detail7"},
		{"detail", "detail8"},
	}
	for _, check := range checks {
		if g[check[0]] == "" || g[check[0]] != g[check[1]] {
			return false
		}
	}
	return true
}

func findThroughputControllerEdit(data []byte, metricsFn string) (claudePatchEdit, bool, error) {
	callbackMatches := controllerCallbackHeaderRe.FindAllSubmatchIndex(data, -1)
	var edits []claudePatchEdit
	for _, callbackMatch := range callbackMatches {
		openBrace := callbackMatch[1] - 1
		blockEnd, ok := findJSBlockEnd(data, openBrace)
		if !ok {
			continue
		}
		const callbackSuffix = `,[]),`
		callbackEnd := blockEnd + len(callbackSuffix)
		if callbackEnd > len(data) || string(data[blockEnd:callbackEnd]) != callbackSuffix {
			continue
		}
		body := data[openBrace+1 : blockEnd-1]
		if !bytes.Contains(body, []byte(metricsFn+"({entries:")) || !bytes.Contains(body, []byte(`subtype:"thinking_tokens"`)) {
			continue
		}
		callback, ok := compactMetricsCallback(
			submatchString(data, callbackMatch, 1),
			submatchString(data, callbackMatch, 2),
			submatchString(data, callbackMatch, 3),
			body,
			metricsFn,
		)
		if !ok {
			continue
		}
		tailMatches := controllerTailRe.FindAllSubmatchIndex(data[callbackEnd:callbackEnd+minInt(4096, len(data)-callbackEnd)], -1)
		if len(tailMatches) != 1 {
			continue
		}
		tailMatch := tailMatches[0]
		groups := namedSubmatches(controllerTailRe, data[callbackEnd:], tailMatch)
		if !controllerTailGroupsConsistent(groups) {
			continue
		}
		tailStart := callbackEnd + tailMatch[0]
		tailEnd := callbackEnd + tailMatch[1]
		setMessagesRe := regexp.MustCompile(`setMessages:(` + claudeIdent + `)`)
		setMessagesMatch := setMessagesRe.FindSubmatch(data[tailEnd : tailEnd+minInt(2048, len(data)-tailEnd)])
		if setMessagesMatch == nil {
			continue
		}
		setMessages := string(setMessagesMatch[1])
		factoryMatches := informationalMessageFactoryRe.FindAllSubmatch(data, -1)
		factoryUseWindow := data[tailEnd : tailEnd+minInt(256*1024, len(data)-tailEnd)]
		var messageFactories []string
		for _, factoryMatch := range factoryMatches {
			candidate := string(factoryMatch[1])
			if bytes.Contains(factoryUseWindow, []byte(candidate+"(")) {
				messageFactories = append(messageFactories, candidate)
			}
		}
		if len(messageFactories) != 1 {
			return claudePatchEdit{}, false, fmt.Errorf("Claude local informational-message factory matched %d targets", len(messageFactories))
		}
		messageFactory := messageFactories[0]
		tail := buildThroughputControllerTail(groups, setMessages, messageFactory)
		replacement := make([]byte, 0, len(callback)+tailStart-callbackEnd+len(tail))
		replacement = append(replacement, callback...)
		replacement = append(replacement, data[callbackEnd:tailStart]...)
		replacement = append(replacement, tail...)
		length := tailEnd - callbackMatch[0]
		if len(replacement) > length {
			return claudePatchEdit{}, false, fmt.Errorf("Claude throughput controller replacement (%d bytes) exceeds target (%d bytes)", len(replacement), length)
		}
		edits = append(edits, claudePatchEdit{offset: callbackMatch[0], length: length, replacement: replacement})
	}
	if len(edits) == 0 {
		return claudePatchEdit{}, false, nil
	}
	if len(edits) != 1 {
		return claudePatchEdit{}, false, fmt.Errorf("Claude throughput controller matched %d targets", len(edits))
	}
	return edits[0], true, nil
}

func compactMetricsCallback(callbackName, react, event string, body []byte, metricsFn string) ([]byte, bool) {
	messageRe := regexp.MustCompile(`if\(` + regexp.QuoteMeta(event) + `\.type==="start"&&` + regexp.QuoteMeta(event) + `\.messageId!=null\)(` + claudeIdent + `)\.current=` + regexp.QuoteMeta(event) + `\.messageId;`)
	messageMatch := messageRe.FindSubmatch(body)
	callRe := regexp.MustCompile(`(` + claudeIdent + `)\.current=` + regexp.QuoteMeta(metricsFn) + `\(\{entries:(` + claudeIdent + `)\.current,responseLength:(` + claudeIdent + `)\.current,event:(` + claudeIdent + `)\}\)`)
	callMatch := callRe.FindSubmatch(body)
	flagRe := regexp.MustCompile(`thinking_signature"&&(` + claudeIdent + `)\(\)\?`)
	flagMatch := flagRe.FindSubmatch(body)
	emitRe := regexp.MustCompile(`(` + claudeIdent + `)\(\{type:"system",subtype:"thinking_tokens",estimated_tokens:`)
	emitMatch := emitRe.FindSubmatch(body)
	if messageMatch == nil || callMatch == nil || flagMatch == nil || emitMatch == nil || string(callMatch[1]) != string(callMatch[3]) || string(callMatch[4]) != event {
		return nil, false
	}
	messageRef := string(messageMatch[1])
	responseRef := string(callMatch[1])
	entriesRef := string(callMatch[2])
	flag := string(flagMatch[1])
	emit := string(emitMatch[1])
	template := `{{CALLBACK}}={{REACT}}.useCallback(t=>{t.type==="start"&&t.messageId!=null&&({{MESSAGE_REF}}.current=t.messageId);let e=()=>{{ENTRIES_REF}}.current.findLast(t=>t.id==null),d=t.type==="thinking_signature"&&{{FLAG}}()?e()?.thinkingTokenEstimate??0:void 0;{{RESPONSE_REF}}.current={{METRICS_FN}}({entries:{{ENTRIES_REF}}.current,responseLength:{{RESPONSE_REF}}.current,event:t});let r=e()?.thinkingTokenEstimate;if(t.type==="thinking_progress"&&{{FLAG}}()&&r!=null){{EMIT}}({type:"system",subtype:"thinking_tokens",estimated_tokens:r,estimated_tokens_delta:t.estimatedTokensDelta});else if(d!==void 0&&r>d){{EMIT}}({type:"system",subtype:"thinking_tokens",estimated_tokens:r,estimated_tokens_delta:r-d})},[]),`
	return []byte(strings.NewReplacer(
		"{{CALLBACK}}", callbackName,
		"{{REACT}}", react,
		"{{MESSAGE_REF}}", messageRef,
		"{{ENTRIES_REF}}", entriesRef,
		"{{RESPONSE_REF}}", responseRef,
		"{{FLAG}}", flag,
		"{{EMIT}}", emit,
		"{{METRICS_FN}}", metricsFn,
	).Replace(template)), true
}

func controllerTailGroupsConsistent(g map[string]string) bool {
	checks := [][2]string{
		{"pref1", "pref2"}, {"motion", "motion2"}, {"motion", "motion3"},
		{"update1", "update2"}, {"update1", "update3"},
		{"buffer1", "buffer2"}, {"buffer1", "buffer3"}, {"buffer1", "buffer4"},
		{"react", "react2"},
	}
	for _, check := range checks {
		if g[check[0]] == "" || g[check[0]] != g[check[1]] {
			return false
		}
	}
	return true
}

func buildThroughputControllerTail(g map[string]string, setMessages, messageFactory string) []byte {
	template := "let {{MOTION}}=!{{SELECT}}(t=>t.settings.prefersReducedMotion)&&!{{DISABLE_MOTION}}(),{{STREAM_CALLBACK}}={{REACT}}.useCallback(t=>{if({{MOTION}}){{BUFFER}}.apply(t);else t({{BUFFER}}.peek())===null&&{{BUFFER}}.clear()},[{{MOTION}},{{BUFFER}}]),{{TURN_END}}={{REACT}}.useCallback(()=>{let s=globalThis.__sr;s&&!s.d&&(s.d=1,s.m&&{{SET_MESSAGES}}(e=>e.concat({{MESSAGE_FACTORY}}(`${s.t.toLocaleString()} output tokens \\xB7 ${(s.t*1e3/s.m).toFixed(1)} tok/s`,\"info\")))),{{RESPONSE_REF}}.current=0,{{ENTRIES_REF}}.current=[],{{MESSAGE_REF}}.current=null},[{{SET_MESSAGES}}]),"
	return []byte(strings.NewReplacer(
		"{{MOTION}}", g["motion"],
		"{{SELECT}}", g["select"],
		"{{DISABLE_MOTION}}", g["disableMotion"],
		"{{STREAM_CALLBACK}}", g["streamCallback"],
		"{{REACT}}", g["react"],
		"{{BUFFER}}", g["buffer1"],
		"{{TURN_END}}", g["turnEnd"],
		"{{SET_MESSAGES}}", setMessages,
		"{{MESSAGE_FACTORY}}", messageFactory,
		"{{RESPONSE_REF}}", g["responseRef"],
		"{{ENTRIES_REF}}", g["entriesRef"],
		"{{MESSAGE_REF}}", g["messageRef"],
	).Replace(template))
}

func namedSubmatches(re *regexp.Regexp, data []byte, match []int) map[string]string {
	groups := make(map[string]string)
	for index, name := range re.SubexpNames() {
		if index == 0 || name == "" || 2*index+1 >= len(match) || match[2*index] < 0 {
			continue
		}
		groups[name] = string(data[match[2*index]:match[2*index+1]])
	}
	return groups
}

func submatchString(data []byte, match []int, group int) string {
	return string(submatchBytes(data, match, group))
}

func submatchBytes(data []byte, match []int, group int) []byte {
	if 2*group+1 >= len(match) || match[2*group] < 0 {
		return nil
	}
	return data[match[2*group]:match[2*group+1]]
}

func freshIdentifiers(count int, used ...string) ([]string, bool) {
	blocked := make(map[string]bool, len(used))
	for _, identifier := range used {
		blocked[identifier] = true
	}
	return chooseFreshIdentifiers(count, blocked)
}

func freshIdentifiersInFunction(function []byte, count int) ([]string, bool) {
	blocked := make(map[string]bool)
	for _, identifier := range jsIdentifierRe.FindAll(function, -1) {
		blocked[string(identifier)] = true
	}
	return chooseFreshIdentifiers(count, blocked)
}

func chooseFreshIdentifiers(count int, blocked map[string]bool) ([]string, bool) {
	candidates := []string{"U", "Q", "J", "S", "X", "Z", "v", "x", "h", "k", "w", "O", "j", "z", "q", "K", "Y", "c", "u", "d", "p", "f", "m", "g", "y", "_", "E", "A", "b", "T", "C", "I", "R", "D", "M", "L", "N", "P", "B", "G", "V", "F", "W"}
	var result []string
	for _, candidate := range candidates {
		if blocked[candidate] {
			continue
		}
		result = append(result, candidate)
		blocked[candidate] = true
		if len(result) == count {
			return result, true
		}
	}
	return nil, false
}

func identifierAppears(data []byte, identifier string) bool {
	if identifier == "" {
		return false
	}
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_$])` + regexp.QuoteMeta(identifier) + `([^A-Za-z0-9_$]|$)`)
	return re.Find(data) != nil
}

func enclosingJSFunction(data []byte, position int) (int, int, bool) {
	searchEnd := position
	for searchEnd > 0 {
		start := bytes.LastIndex(data[:searchEnd], []byte("function "))
		if start < 0 {
			return 0, 0, false
		}
		openRel := bytes.IndexByte(data[start:position], '{')
		if openRel >= 0 {
			if end, ok := findJSBlockEnd(data, start+openRel); ok && end > position {
				return start, end, true
			}
		}
		searchEnd = start
	}
	return 0, 0, false
}

// findJSBlockEnd scans a minified function while ignoring braces in strings
// and comments. Claude's relevant templates do not contain nested backticks;
// treating the complete template as a string therefore preserves brace depth.
func findJSBlockEnd(data []byte, openBrace int) (int, bool) {
	if openBrace < 0 || openBrace >= len(data) || data[openBrace] != '{' {
		return 0, false
	}
	depth := 0
	quote := byte(0)
	lineComment := false
	blockComment := false
	for i := openBrace; i < len(data); i++ {
		ch := data[i]
		if lineComment {
			if ch == '\n' || ch == '\r' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if ch == '*' && i+1 < len(data) && data[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if ch == '\\' {
				i++
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '/' && i+1 < len(data) {
			switch data[i+1] {
			case '/':
				lineComment = true
				i++
				continue
			case '*':
				blockComment = true
				i++
				continue
			}
		}
		switch ch {
		case '\'', '"', '`':
			quote = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
