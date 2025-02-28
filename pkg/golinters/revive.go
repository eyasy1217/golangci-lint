package golinters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/token"
	"os"
	"reflect"
	"sync"

	"github.com/BurntSushi/toml"
	reviveConfig "github.com/mgechev/revive/config"
	"github.com/mgechev/revive/lint"
	"github.com/mgechev/revive/rule"
	"golang.org/x/tools/go/analysis"

	"github.com/golangci/golangci-lint/pkg/config"
	"github.com/golangci/golangci-lint/pkg/golinters/goanalysis"
	"github.com/golangci/golangci-lint/pkg/lint/linter"
	"github.com/golangci/golangci-lint/pkg/logutils"
	"github.com/golangci/golangci-lint/pkg/result"
)

const reviveName = "revive"

var reviveDebugf = logutils.Debug(logutils.DebugKeyRevive)

// jsonObject defines a JSON object of a failure
type jsonObject struct {
	Severity     lint.Severity
	lint.Failure `json:",inline"`
}

// NewRevive returns a new Revive linter.
//

func NewRevive(settings *config.ReviveSettings) *goanalysis.Linter {
	var mu sync.Mutex
	var resIssues []goanalysis.Issue

	analyzer := &analysis.Analyzer{
		Name: goanalysis.TheOnlyAnalyzerName,
		Doc:  goanalysis.TheOnlyanalyzerDoc,
		Run:  goanalysis.DummyRun,
	}

	return goanalysis.NewLinter(
		reviveName,
		"Fast, configurable, extensible, flexible, and beautiful linter for Go. Drop-in replacement of golint.",
		[]*analysis.Analyzer{analyzer},
		nil,
	).WithContextSetter(func(lintCtx *linter.Context) {
		analyzer.Run = func(pass *analysis.Pass) (any, error) {
			issues, err := runRevive(lintCtx, pass, settings)
			if err != nil {
				return nil, err
			}

			if len(issues) == 0 {
				return nil, nil
			}

			mu.Lock()
			resIssues = append(resIssues, issues...)
			mu.Unlock()

			return nil, nil
		}
	}).WithIssuesReporter(func(*linter.Context) []goanalysis.Issue {
		return resIssues
	}).WithLoadMode(goanalysis.LoadModeSyntax)
}

func runRevive(lintCtx *linter.Context, pass *analysis.Pass, settings *config.ReviveSettings) ([]goanalysis.Issue, error) {
	packages := [][]string{getFileNames(pass)}

	conf, err := getReviveConfig(settings)
	if err != nil {
		return nil, err
	}

	formatter, err := reviveConfig.GetFormatter("json")
	if err != nil {
		return nil, err
	}

	revive := lint.New(os.ReadFile, settings.MaxOpenFiles)

	lintingRules, err := reviveConfig.GetLintingRules(conf, []lint.Rule{})
	if err != nil {
		return nil, err
	}

	failures, err := revive.Lint(packages, lintingRules, *conf)
	if err != nil {
		return nil, err
	}

	formatChan := make(chan lint.Failure)
	exitChan := make(chan bool)

	var output string
	go func() {
		output, err = formatter.Format(formatChan, *conf)
		if err != nil {
			lintCtx.Log.Errorf("Format error: %v", err)
		}
		exitChan <- true
	}()

	for f := range failures {
		if f.Confidence < conf.Confidence {
			continue
		}

		formatChan <- f
	}

	close(formatChan)
	<-exitChan

	var results []jsonObject
	err = json.Unmarshal([]byte(output), &results)
	if err != nil {
		return nil, err
	}

	var issues []goanalysis.Issue
	for i := range results {
		issues = append(issues, reviveToIssue(pass, &results[i]))
	}

	return issues, nil
}

func reviveToIssue(pass *analysis.Pass, object *jsonObject) goanalysis.Issue {
	lineRangeTo := object.Position.End.Line
	if object.RuleName == (&rule.ExportedRule{}).Name() {
		lineRangeTo = object.Position.Start.Line
	}

	return goanalysis.NewIssue(&result.Issue{
		Severity: string(object.Severity),
		Text:     fmt.Sprintf("%s: %s", object.RuleName, object.Failure.Failure),
		Pos: token.Position{
			Filename: object.Position.Start.Filename,
			Line:     object.Position.Start.Line,
			Offset:   object.Position.Start.Offset,
			Column:   object.Position.Start.Column,
		},
		LineRange: &result.Range{
			From: object.Position.Start.Line,
			To:   lineRangeTo,
		},
		FromLinter: reviveName,
	}, pass)
}

// This function mimics the GetConfig function of revive.
// This allows to get default values and right types.
// https://github.com/golangci/golangci-lint/issues/1745
// https://github.com/mgechev/revive/blob/v1.1.4/config/config.go#L182
func getReviveConfig(cfg *config.ReviveSettings) (*lint.Config, error) {
	conf := defaultConfig()

	if !reflect.DeepEqual(cfg, &config.ReviveSettings{}) {
		rawRoot := createConfigMap(cfg)
		buf := bytes.NewBuffer(nil)

		err := toml.NewEncoder(buf).Encode(rawRoot)
		if err != nil {
			return nil, fmt.Errorf("failed to encode configuration: %w", err)
		}

		conf = &lint.Config{}
		_, err = toml.NewDecoder(buf).Decode(conf)
		if err != nil {
			return nil, fmt.Errorf("failed to decode configuration: %w", err)
		}
	}

	normalizeConfig(conf)

	reviveDebugf("revive configuration: %#v", conf)

	return conf, nil
}

func createConfigMap(cfg *config.ReviveSettings) map[string]any {
	rawRoot := map[string]any{
		"ignoreGeneratedHeader": cfg.IgnoreGeneratedHeader,
		"confidence":            cfg.Confidence,
		"severity":              cfg.Severity,
		"errorCode":             cfg.ErrorCode,
		"warningCode":           cfg.WarningCode,
		"enableAllRules":        cfg.EnableAllRules,
	}

	rawDirectives := map[string]map[string]any{}
	for _, directive := range cfg.Directives {
		rawDirectives[directive.Name] = map[string]any{
			"severity": directive.Severity,
		}
	}

	if len(rawDirectives) > 0 {
		rawRoot["directive"] = rawDirectives
	}

	rawRules := map[string]map[string]any{}
	for _, s := range cfg.Rules {
		rawRules[s.Name] = map[string]any{
			"severity":  s.Severity,
			"arguments": safeTomlSlice(s.Arguments),
			"disabled":  s.Disabled,
		}
	}

	if len(rawRules) > 0 {
		rawRoot["rule"] = rawRules
	}

	return rawRoot
}

func safeTomlSlice(r []any) []any {
	if len(r) == 0 {
		return nil
	}

	if _, ok := r[0].(map[any]any); !ok {
		return r
	}

	var typed []any
	for _, elt := range r {
		item := map[string]any{}
		for k, v := range elt.(map[any]any) {
			item[k.(string)] = v
		}

		typed = append(typed, item)
	}

	return typed
}

// This element is not exported by revive, so we need copy the code.
// Extracted from https://github.com/mgechev/revive/blob/v1.3.5/config/config.go#L15
var defaultRules = []lint.Rule{
	&rule.VarDeclarationsRule{},
	&rule.PackageCommentsRule{},
	&rule.DotImportsRule{},
	&rule.BlankImportsRule{},
	&rule.ExportedRule{},
	&rule.VarNamingRule{},
	&rule.IndentErrorFlowRule{},
	&rule.RangeRule{},
	&rule.ErrorfRule{},
	&rule.ErrorNamingRule{},
	&rule.ErrorStringsRule{},
	&rule.ReceiverNamingRule{},
	&rule.IncrementDecrementRule{},
	&rule.ErrorReturnRule{},
	&rule.UnexportedReturnRule{},
	&rule.TimeNamingRule{},
	&rule.ContextKeysType{},
	&rule.ContextAsArgumentRule{},
	&rule.EmptyBlockRule{},
	&rule.SuperfluousElseRule{},
	&rule.UnusedParamRule{},
	&rule.UnreachableCodeRule{},
	&rule.RedefinesBuiltinIDRule{},
}

var allRules = append([]lint.Rule{
	&rule.ArgumentsLimitRule{},
	&rule.CyclomaticRule{},
	&rule.FileHeaderRule{},
	&rule.ConfusingNamingRule{},
	&rule.GetReturnRule{},
	&rule.ModifiesParamRule{},
	&rule.ConfusingResultsRule{},
	&rule.DeepExitRule{},
	&rule.AddConstantRule{},
	&rule.FlagParamRule{},
	&rule.UnnecessaryStmtRule{},
	&rule.StructTagRule{},
	&rule.ModifiesValRecRule{},
	&rule.ConstantLogicalExprRule{},
	&rule.BoolLiteralRule{},
	&rule.ImportsBlacklistRule{},
	&rule.FunctionResultsLimitRule{},
	&rule.MaxPublicStructsRule{},
	&rule.RangeValInClosureRule{},
	&rule.RangeValAddress{},
	&rule.WaitGroupByValueRule{},
	&rule.AtomicRule{},
	&rule.EmptyLinesRule{},
	&rule.LineLengthLimitRule{},
	&rule.CallToGCRule{},
	&rule.DuplicatedImportsRule{},
	&rule.ImportShadowingRule{},
	&rule.BareReturnRule{},
	&rule.UnusedReceiverRule{},
	&rule.UnhandledErrorRule{},
	&rule.CognitiveComplexityRule{},
	&rule.StringOfIntRule{},
	&rule.StringFormatRule{},
	&rule.EarlyReturnRule{},
	&rule.UnconditionalRecursionRule{},
	&rule.IdenticalBranchesRule{},
	&rule.DeferRule{},
	&rule.UnexportedNamingRule{},
	&rule.FunctionLength{},
	&rule.NestedStructs{},
	&rule.UselessBreak{},
	&rule.UncheckedTypeAssertionRule{},
	&rule.TimeEqualRule{},
	&rule.BannedCharsRule{},
	&rule.OptimizeOperandsOrderRule{},
	&rule.UseAnyRule{},
	&rule.DataRaceRule{},
	&rule.CommentSpacingsRule{},
	&rule.IfReturnRule{},
	&rule.RedundantImportAlias{},
	&rule.ImportAliasNamingRule{},
	&rule.EnforceMapStyleRule{},
	&rule.EnforceRepeatedArgTypeStyleRule{},
	&rule.EnforceSliceStyleRule{},
}, defaultRules...)

const defaultConfidence = 0.8

// This element is not exported by revive, so we need copy the code.
// Extracted from https://github.com/mgechev/revive/blob/v1.1.4/config/config.go#L145
func normalizeConfig(cfg *lint.Config) {
	// NOTE(ldez): this custom section for golangci-lint should be kept.
	// ---
	if cfg.Confidence == 0 {
		cfg.Confidence = defaultConfidence
	}
	if cfg.Severity == "" {
		cfg.Severity = lint.SeverityWarning
	}
	// ---

	if len(cfg.Rules) == 0 {
		cfg.Rules = map[string]lint.RuleConfig{}
	}
	if cfg.EnableAllRules {
		// Add to the configuration all rules not yet present in it
		for _, r := range allRules {
			ruleName := r.Name()
			_, alreadyInConf := cfg.Rules[ruleName]
			if alreadyInConf {
				continue
			}
			// Add the rule with an empty conf for
			cfg.Rules[ruleName] = lint.RuleConfig{}
		}
	}

	severity := cfg.Severity
	if severity != "" {
		for k, v := range cfg.Rules {
			if v.Severity == "" {
				v.Severity = severity
			}
			cfg.Rules[k] = v
		}
		for k, v := range cfg.Directives {
			if v.Severity == "" {
				v.Severity = severity
			}
			cfg.Directives[k] = v
		}
	}
}

// This element is not exported by revive, so we need copy the code.
// Extracted from https://github.com/mgechev/revive/blob/v1.1.4/config/config.go#L214
func defaultConfig() *lint.Config {
	defaultConfig := lint.Config{
		Confidence: defaultConfidence,
		Severity:   lint.SeverityWarning,
		Rules:      map[string]lint.RuleConfig{},
	}
	for _, r := range defaultRules {
		defaultConfig.Rules[r.Name()] = lint.RuleConfig{}
	}
	return &defaultConfig
}
