package internal

import (
	"errors"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type shellStatusSet uint8

const (
	shellStatusMaySucceed shellStatusSet = 1 << iota
	shellStatusMayFail
	shellStatusEither = shellStatusMaySucceed | shellStatusMayFail
)

type shellCommandSummary struct {
	statuses     shellStatusSet
	changesState bool
}

type shellCommandResult struct {
	status shellCommandStatus
	safe   bool
}

type shellEvaluation struct {
	statuses shellStatusSet
	safe     bool
}

func parseShellCommandList(source string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(source), "")
}

func supportedShellCommandList(file *syntax.File, makeCommand string) bool {
	for _, statement := range file.Stmts {
		if !supportedShellStatement(statement) {
			if _, ok := redirectedShellStatementSummary(statement, makeCommand); !ok {
				return false
			}
		}
	}
	return true
}

func redirectedShellStatementSummary(statement *syntax.Stmt, makeCommand string) (shellEvaluation, bool) {
	if statement == nil || statement.Negated || statement.Background || statement.Coprocess || statement.Disown ||
		len(statement.Redirs) == 0 {
		return shellEvaluation{}, false
	}
	command, ok := statement.Cmd.(*syntax.CallExpr)
	if !ok || len(command.Args) == 0 || !supportedShellCall(command) {
		return shellEvaluation{}, false
	}
	name, static := staticShellWord(command.Args[0])
	base := executableBase(name)
	configuredMake := makeCommand != "" && base == executableBase(makeCommand)
	if !static || !trackableShellPathWord(command.Args[0]) || name == ":" || name == "times" || name == "cd" ||
		isMakeExecutable(name) ||
		strings.HasSuffix(base, "-make") || configuredMake || base == "mkdir" {
		return shellEvaluation{}, false
	}
	for _, redirect := range statement.Redirs {
		if !supportedShellRedirect(redirect) {
			return shellEvaluation{}, false
		}
	}
	return shellEvaluation{statuses: shellStatusEither, safe: true}, true
}

func supportedShellRedirect(redirect *syntax.Redirect) bool {
	if redirect == nil || redirect.Word == nil || redirect.Hdoc != nil {
		return false
	}
	value, static := staticShellWord(redirect.Word)
	if !static || !trackableShellPathWord(redirect.Word) {
		return false
	}
	switch redirect.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrIn, syntax.RdrInOut, syntax.RdrClob:
		return true
	case syntax.DplIn, syntax.DplOut:
		return value == "0" || value == "1" || value == "2" || value == "-"
	default:
		return false
	}
}

func supportedShellStatement(statement *syntax.Stmt) bool {
	if statement == nil || statement.Negated || statement.Background || statement.Coprocess || statement.Disown ||
		len(statement.Redirs) != 0 {
		return false
	}

	switch command := statement.Cmd.(type) {
	case *syntax.CallExpr:
		return supportedShellCall(command)
	case *syntax.BinaryCmd:
		return (command.Op == syntax.AndStmt || command.Op == syntax.OrStmt) &&
			supportedShellStatement(command.X) && supportedShellStatement(command.Y)
	default:
		return false
	}
}

func supportedShellCall(command *syntax.CallExpr) bool {
	if len(command.Args) == 0 {
		return false
	}
	if name, ok := staticShellWord(command.Args[0]); ok {
		if shellControlCall(name) {
			return false
		}
		if name == ":" && len(command.Assigns) != 0 {
			return false
		}
		switch {
		case name == "cd":
			if len(command.Assigns) != 0 || len(command.Args) != 2 || !trackableShellPathWord(command.Args[1]) {
				return false
			}
			if value, static := staticShellWord(command.Args[1]); static && (value == "" || value == "-") {
				return false
			}
		case executableBase(name) == "mkdir":
			if len(command.Assigns) != 0 {
				return false
			}
			for _, argument := range command.Args[1:] {
				if !trackableShellPathWord(argument) {
					return false
				}
			}
		case isMakeExecutable(name):
			if len(command.Assigns) != 0 || !trackableMakeDirectoryArguments(command.Args[1:]) {
				return false
			}
		}
	}

	supported := true
	syntax.Walk(command, func(node syntax.Node) bool {
		switch node := node.(type) {
		case *syntax.CmdSubst:
			if !node.Backquotes {
				supported = false
			}
			return false
		case *syntax.ParamExp:
			if !simpleShellParameterExpansion(node) {
				supported = false
			}
			return supported
		case *syntax.ArithmExp, *syntax.ProcSubst:
			supported = false
			return false
		default:
			return supported
		}
	})
	return supported
}

func simpleShellParameterExpansion(expansion *syntax.ParamExp) bool {
	return expansion.Param != nil && expansion.Flags == nil && !expansion.Excl && !expansion.Length &&
		!expansion.Width && !expansion.IsSet && expansion.NestedParam == nil && expansion.Index == nil &&
		len(expansion.Modifiers) == 0 && expansion.Slice == nil && expansion.Repl == nil &&
		expansion.Names == 0 && expansion.Exp == nil
}

func trackableShellPathWord(word *syntax.Word) bool {
	first := true
	var checkParts func([]syntax.WordPart, bool) bool
	checkParts = func(parts []syntax.WordPart, quoted bool) bool {
		for _, part := range parts {
			switch part := part.(type) {
			case *syntax.Lit:
				for index := 0; index < len(part.Value); index++ {
					character := part.Value[index]
					if !quoted && character == '\\' && index+1 < len(part.Value) {
						index++
						first = false
						continue
					}
					if !quoted && (strings.ContainsRune("*?[", rune(character)) || first && character == '~') {
						return false
					}
					first = false
				}
			case *syntax.SglQuoted:
				first = false
			case *syntax.DblQuoted:
				if !checkParts(part.Parts, true) {
					return false
				}
				first = false
			case *syntax.CmdSubst:
				if !part.Backquotes {
					return false
				}
				first = false
			default:
				return false
			}
		}
		return true
	}
	return checkParts(word.Parts, false)
}

func trackableMakeDirectoryArguments(arguments []*syntax.Word) bool {
	for i := 0; i < len(arguments); i++ {
		argument, static := staticShellWord(arguments[i])
		if !static {
			return false
		}
		spec := classifyMakeArgument(argument)
		if !spec.valid {
			return false
		}
		if spec.stop {
			return true
		}
		if !spec.option {
			if !safeMakeTargetWord(arguments[i], argument) {
				return false
			}
			continue
		}
		if spec.valueMode == makeOptionArgumentNone ||
			spec.valueMode == makeOptionArgumentOptionalAttached && !spec.valueInline {
			continue
		}
		if !spec.valueInline {
			i++
			if i >= len(arguments) {
				return false
			}
			if spec.directory {
				if !trackableMakeDirectoryWord(arguments[i]) {
					return false
				}
			} else {
				value, static := staticShellWord(arguments[i])
				if !static || !safeMakeTargetWord(arguments[i], value) {
					return false
				}
			}
		} else if spec.directory {
			if !trackableStaticMakeDirectory(spec.inlineValue) {
				return false
			}
		}
	}
	return true
}

func safeMakeTargetWord(word *syntax.Word, value string) bool {
	if trackableShellPathWord(word) {
		return true
	}
	firstPattern := strings.IndexAny(value, "*?[")
	return firstPattern > 0 && value[0] != '-'
}

func trackableMakeDirectoryWord(word *syntax.Word) bool {
	if !trackableShellPathWord(word) {
		return false
	}
	value, static := staticShellWord(word)
	return !static || trackableStaticMakeDirectory(value)
}

func trackableStaticMakeDirectory(value string) bool {
	return value != "" && !strings.HasPrefix(value, "~") && !strings.ContainsAny(value, "*?[")
}

func staticShellWord(word *syntax.Word) (string, bool) {
	raw, ok := rawStaticShellWord(word)
	if !ok {
		return "", false
	}
	if isRawWindowsAbsolutePathToken(raw) {
		return raw, true
	}

	var value strings.Builder
	var appendParts func([]syntax.WordPart, bool) bool
	appendParts = func(parts []syntax.WordPart, quoted bool) bool {
		for _, part := range parts {
			switch part := part.(type) {
			case *syntax.Lit:
				for index := 0; index < len(part.Value); index++ {
					if part.Value[index] == '\\' && !quoted && index+1 < len(part.Value) {
						index++
					}
					value.WriteByte(part.Value[index])
				}
			case *syntax.SglQuoted:
				value.WriteString(part.Value)
			case *syntax.DblQuoted:
				if !appendParts(part.Parts, true) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !appendParts(word.Parts, false) {
		return "", false
	}
	return value.String(), true
}

func rawStaticShellWord(word *syntax.Word) (string, bool) {
	var value strings.Builder
	var appendParts func([]syntax.WordPart) bool
	appendParts = func(parts []syntax.WordPart) bool {
		for _, part := range parts {
			switch part := part.(type) {
			case *syntax.Lit:
				value.WriteString(part.Value)
			case *syntax.SglQuoted:
				value.WriteString(part.Value)
			case *syntax.DblQuoted:
				if !appendParts(part.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !appendParts(word.Parts) {
		return "", false
	}
	return value.String(), true
}

func shellControlCall(name string) bool {
	switch name {
	case ".", "break", "builtin", "command", "continue", "eval", "exec", "exit", "export", "read",
		"readonly", "return", "set", "shift", "source", "trap", "unset":
		return true
	default:
		return false
	}
}

func evaluateShellCommandList(
	file *syntax.File,
	source string,
	makeCommand string,
	process func(string, string, bool) shellCommandResult,
	summarize func(string, string, bool) shellCommandSummary,
	unknownConditional func(),
	statementEvaluated func(shellEvaluation),
) bool {
	for _, statement := range file.Stmts {
		var result shellEvaluation
		if len(statement.Redirs) != 0 {
			var ok bool
			result, ok = redirectedShellStatementSummary(statement, makeCommand)
			if !ok {
				return false
			}
		} else {
			result = evaluateShellStatement(statement, source, process, summarize, unknownConditional)
		}
		statementEvaluated(result)
		if !result.safe {
			return false
		}
	}
	return true
}

func evaluateShellStatement(
	statement *syntax.Stmt,
	source string,
	process func(string, string, bool) shellCommandResult,
	summarize func(string, string, bool) shellCommandSummary,
	unknownConditional func(),
) shellEvaluation {
	switch command := statement.Cmd.(type) {
	case *syntax.CallExpr:
		commandText, ok := shellCommandText(command, source)
		if !ok {
			return shellEvaluation{statuses: shellStatusEither, safe: false}
		}
		commandName, staticName := staticShellWord(command.Args[0])
		result := process(commandText, commandName, staticName)
		return shellEvaluation{statuses: shellStatusSetFromStatus(result.status), safe: result.safe}
	case *syntax.BinaryCmd:
		left := evaluateShellStatement(command.X, source, process, summarize, unknownConditional)
		if !left.safe {
			return left
		}
		switch command.Op {
		case syntax.AndStmt:
			if left.statuses == shellStatusMaySucceed {
				return evaluateShellStatement(command.Y, source, process, summarize, unknownConditional)
			}
			if left.statuses == shellStatusMayFail {
				return left
			}
		case syntax.OrStmt:
			if left.statuses == shellStatusMayFail {
				return evaluateShellStatement(command.Y, source, process, summarize, unknownConditional)
			}
			if left.statuses == shellStatusMaySucceed {
				return left
			}
		}
		unknownConditional()
		right := summarizeShellStatement(command.Y, source, summarize)
		return shellEvaluation{
			statuses: combineShellStatuses(command.Op, left.statuses, right.statuses),
			safe:     right.safe,
		}
	}
	return shellEvaluation{statuses: shellStatusEither, safe: false}
}

func summarizeShellStatement(
	statement *syntax.Stmt,
	source string,
	summarize func(string, string, bool) shellCommandSummary,
) shellEvaluation {
	switch command := statement.Cmd.(type) {
	case *syntax.CallExpr:
		commandText, ok := shellCommandText(command, source)
		if !ok {
			return shellEvaluation{statuses: shellStatusEither, safe: false}
		}
		commandName, staticName := staticShellWord(command.Args[0])
		summary := summarize(commandText, commandName, staticName)
		if !staticName {
			summary.changesState = true
		}
		if summary.statuses == 0 {
			summary.statuses = shellStatusEither
		}
		return shellEvaluation{statuses: summary.statuses, safe: !summary.changesState}
	case *syntax.BinaryCmd:
		left := summarizeShellStatement(command.X, source, summarize)
		if !left.safe {
			return left
		}
		trigger := shellStatusMaySucceed
		if command.Op == syntax.OrStmt {
			trigger = shellStatusMayFail
		}
		if left.statuses&trigger == 0 {
			return left
		}
		right := summarizeShellStatement(command.Y, source, summarize)
		return shellEvaluation{
			statuses: combineShellStatuses(command.Op, left.statuses, right.statuses),
			safe:     right.safe,
		}
	default:
		return shellEvaluation{statuses: shellStatusEither, safe: false}
	}
}

func shellCommandText(command *syntax.CallExpr, source string) (string, bool) {
	start := int(command.Pos().Offset())
	end := int(command.End().Offset())
	if start < 0 || end < start || end > len(source) {
		return "", false
	}
	return strings.TrimSpace(source[start:end]), true
}

func shellStatusSetFromStatus(status shellCommandStatus) shellStatusSet {
	switch status {
	case shellStatusSuccess:
		return shellStatusMaySucceed
	case shellStatusFailure:
		return shellStatusMayFail
	default:
		return shellStatusEither
	}
}

func combineShellStatuses(operator syntax.BinCmdOperator, left, right shellStatusSet) shellStatusSet {
	var result shellStatusSet
	switch operator {
	case syntax.AndStmt:
		if left&shellStatusMayFail != 0 {
			result |= shellStatusMayFail
		}
		if left&shellStatusMaySucceed != 0 {
			result |= right
		}
	case syntax.OrStmt:
		if left&shellStatusMaySucceed != 0 {
			result |= shellStatusMaySucceed
		}
		if left&shellStatusMayFail != 0 {
			result |= right
		}
	}
	return result
}

func shellParseError(parseErr error) *shellTokenizationError {
	var syntaxErr syntax.ParseError
	if errors.As(parseErr, &syntaxErr) {
		return &shellTokenizationError{offset: int(syntaxErr.Pos.Offset()), reason: "invalid shell syntax"}
	}
	var languageErr syntax.LangError
	if errors.As(parseErr, &languageErr) {
		return &shellTokenizationError{offset: int(languageErr.Pos.Offset()), reason: "unsupported shell syntax"}
	}
	return &shellTokenizationError{reason: "invalid shell syntax"}
}

func shellDiagnosticSegments(source string) []string {
	segments := []string{}
	start := 0
	var quote byte
	escaped := false
	backtick := false
	flush := func(end int) {
		if segment := strings.TrimSpace(source[start:end]); segment != "" {
			segments = append(segments, segment)
		}
	}
	for i := 0; i < len(source); i++ {
		character := source[i]
		if escaped {
			escaped = false
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if backtick {
			if character == '`' {
				backtick = false
			}
			continue
		}
		switch character {
		case '\'':
			quote = character
		case '"':
			if quote == character {
				quote = 0
			} else if quote == 0 {
				quote = character
			}
		case '`':
			backtick = true
		case '#':
			if quote == 0 && (i == start || i > 0 && (source[i-1] == ' ' || source[i-1] == '\t')) {
				flush(i)
				return segments
			}
		case ';', '|', '&':
			if quote != 0 {
				continue
			}
			flush(i)
			if i+1 < len(source) && source[i+1] == character {
				i++
			}
			start = i + 1
		}
	}
	flush(len(source))
	return segments
}

func compatibleMakeDirectoryMarker(file *syntax.File, parseErr error, source, makeCommand string) string {
	if parseErr == nil && !singleMakeDirectoryMarker(file) {
		return ""
	}
	trimmed := strings.TrimSpace(source)
	marker := ": Entering directory "
	index := strings.Index(trimmed, marker)
	if index < 0 {
		marker = ": Leaving directory "
		index = strings.Index(trimmed, marker)
	}
	if index < 0 || strings.ContainsAny(trimmed[:index], "#;&|") {
		return ""
	}
	if _, ok := makeDirectoryEvent(trimmed, "Entering", makeCommand); !ok {
		if _, ok := makeDirectoryEvent(trimmed, "Leaving", makeCommand); !ok {
			return ""
		}
	}
	return trimmed
}

func singleMakeDirectoryMarker(file *syntax.File) bool {
	if file == nil || len(file.Stmts) != 1 {
		return false
	}
	statement := file.Stmts[0]
	if statement == nil || statement.Negated || statement.Background || statement.Coprocess || statement.Disown ||
		len(statement.Redirs) != 0 {
		return false
	}
	command, ok := statement.Cmd.(*syntax.CallExpr)
	return ok && len(command.Args) == 4
}
