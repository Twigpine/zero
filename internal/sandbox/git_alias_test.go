package sandbox

import (
	"reflect"
	"testing"
)

// The tokenizer has to agree with git's split_cmdline on the forms that decide
// which subcommand runs: quotes anywhere in a word, backslash escapes outside
// single quotes, and the two errors git dies on.
func TestSplitGitAliasExpansionMatchesGitQuoting(t *testing.T) {
	for _, tc := range []struct {
		expansion string
		want      []string
		ok        bool
	}{
		{"init", []string{"init"}, true},
		{`"init"`, []string{"init"}, true},
		{`in"it"`, []string{"init"}, true},
		{`in\it`, []string{"init"}, true},
		{`'in\it'`, []string{`in\it`}, true},
		{`"in\"it"`, []string{`in"it`}, true},
		{`-c alias.b=init b`, []string{"-c", "alias.b=init", "b"}, true},
		{`-c "alias.b=init x" b`, []string{"-c", "alias.b=init x", "b"}, true},
		{`'-c' 'alias.b=init' b`, []string{"-c", "alias.b=init", "b"}, true},
		{"  init \t ", []string{"init"}, true},
		{"", nil, true},
		{`"init`, nil, false},
		{`'init`, nil, false},
		{`init\`, nil, false},
	} {
		got, ok := splitGitAliasExpansion(tc.expansion)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitGitAliasExpansion(%q) = %q, %v; want %q, %v", tc.expansion, got, ok, tc.want, tc.ok)
		}
	}
}

// GIT_CONFIG_PARAMETERS is git's own single-quoted list; an entry git would
// call bogus makes the whole variable unreadable rather than partly read.
func TestSplitGitConfigParametersDequotesLikeGit(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
		ok   bool
	}{
		{`'alias.a=init'`, []string{"alias.a=init"}, true},
		{`'alias.a=init' 'x=y'`, []string{"alias.a=init", "x=y"}, true},
		{` 'alias.a=in'\''it' `, []string{"alias.a=in'it"}, true},
		{`'alias.a=in'\!'it'`, []string{"alias.a=in!it"}, true},
		{"", nil, true},
		{`alias.a=init`, nil, false},
		{`'alias.a=init`, nil, false},
		{`'a'x`, nil, false},
	} {
		got, ok := splitGitConfigParameters(tc.text)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitGitConfigParameters(%q) = %q, %v; want %q, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

// Resolution applies git's in-alias option handling: a -c inside an expansion
// defines the alias the next lookup uses, pager switches are stepped over, an
// environment-changing option or an unreadable definition refuses, and only a
// chain that ends in a real repository-creating command is creation.
func TestGitAliasResolutionFollowsExpansionOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		subcommand string
		aliases    map[string]string
		unknown    []string
		creates    bool
	}{
		{name: "option inside expansion defines the next alias", subcommand: "a", aliases: map[string]string{"a": "-c alias.b=init b"}, creates: true},
		{name: "quoted expansion", subcommand: "a", aliases: map[string]string{"a": `"init"`}, creates: true},
		{name: "pager switch does not hide init", subcommand: "a", aliases: map[string]string{"a": "--no-pager init"}, creates: true},
		{name: "environment-changing option refuses", subcommand: "a", aliases: map[string]string{"a": "-C . init"}, creates: true},
		{name: "unclosed quote refuses", subcommand: "a", aliases: map[string]string{"a": `"in`}, creates: true},
		{name: "options only refuses", subcommand: "a", aliases: map[string]string{"a": "-c x=y"}, creates: true},
		{name: "config-env inside expansion is unreadable", subcommand: "a", aliases: map[string]string{"a": "--config-env=alias.b=var b"}, creates: true},
		{name: "redefinition loop refuses", subcommand: "a", aliases: map[string]string{"a": "-c alias.a=init a"}, creates: true},
		{name: "unknown definition refuses", subcommand: "a", unknown: []string{"a"}, creates: true},
		{name: "harmless option-bearing alias", subcommand: "st", aliases: map[string]string{"st": "-c color.ui=always status"}, creates: false},
		{name: "harmless quoted alias", subcommand: "st", aliases: map[string]string{"st": `"status"`}, creates: false},
		{name: "harmless pager alias", subcommand: "l", aliases: map[string]string{"l": "--no-pager log --oneline"}, creates: false},
		{name: "harmless chain through quotes", subcommand: "s", aliases: map[string]string{"s": "st", "st": `sta"tus"`}, creates: false},
		{name: "not an alias", subcommand: "status", creates: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := gitAliasTable{}
			for name, expansion := range tc.aliases {
				table.define(name, expansion)
			}
			for _, name := range tc.unknown {
				table.defineUnknown(name)
			}
			if got := gitSubcommandCreatesRepository(tc.subcommand, table); got != tc.creates {
				t.Fatalf("gitSubcommandCreatesRepository(%q) = %v, want %v", tc.subcommand, got, tc.creates)
			}
		})
	}
}
