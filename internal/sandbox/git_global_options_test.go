package sandbox

import "testing"

// GIT'S GLOBAL OPTIONS CONSUME THE NEXT WORD, AND THE CLASSIFIER HAS TO KNOW.
//
// firstSubcommand skips words beginning with "-" but not the value that follows
// one. So "git -C sub clone <url>" made it answer "sub", the command classified
// as touching no network at all, and the network gate never fired. Two spellings
// an ordinary user types by habit walked straight through it.
//
// Driven through AnalyzeCommand rather than the helper, because the gate reads
// the analysis and a helper-level assertion would not have caught this.
func TestGitGlobalOptionsDoNotHideTheSubcommand(t *testing.T) {
	for _, testCase := range []struct {
		script string
		want   bool
	}{
		{script: "git clone https://example.com/x", want: true},
		{script: "git -C sub clone https://example.com/x", want: true},
		{script: "git -c user.name=x clone https://example.com/x", want: true},
		{script: "git -c a=b -C sub clone https://example.com/x", want: true},
		{script: "git --git-dir=/tmp/g clone https://example.com/x", want: true},
		{script: "git --namespace ns fetch origin", want: true},
		{script: "git -C sub push origin main", want: true},
		// Local-only work stays local, or the fix would be "call everything network".
		{script: "git status", want: false},
		{script: "git -C sub status", want: false},
		{script: "git -c a=b commit -m x", want: false},
		{script: "git init", want: false},
	} {
		if got := AnalyzeCommand(testCase.script).Network; got != testCase.want {
			t.Errorf("AnalyzeCommand(%q).Network = %v, want %v", testCase.script, got, testCase.want)
		}
	}
}

// The subcommand parser itself, including the option that consumes a value and
// the self-contained "--opt=value" spelling that does not.
func TestGitSubcommandSkipsOptionValues(t *testing.T) {
	for _, testCase := range []struct {
		words []string
		want  string
	}{
		{words: []string{"clone", "url"}, want: "clone"},
		{words: []string{"-c", "sub", "clone"}, want: "clone"},
		{words: []string{"--git-dir=/tmp/g", "clone"}, want: "clone"},
		{words: []string{"--namespace", "ns", "fetch"}, want: "fetch"},
		{words: []string{"-c", "a=b", "-c", "sub", "push"}, want: "push"},
		{words: []string{"--no-pager", "status"}, want: "status"},
		{words: []string{}, want: ""},
		{words: []string{"-c", "a=b"}, want: ""},
	} {
		if got := gitSubcommand(testCase.words); got != testCase.want {
			t.Errorf("gitSubcommand(%v) = %q, want %q", testCase.words, got, testCase.want)
		}
	}
}
