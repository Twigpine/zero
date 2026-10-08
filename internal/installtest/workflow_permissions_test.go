package installtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPullRequestWorkflowsDeclareContentsRead(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		rel := filepath.Join(".github", "workflows", e.Name())
		body := readRepoText(t, rel)
		doc, err := parseWorkflow(body)
		if err != nil {
			t.Errorf("%s: yaml: %v", rel, err)
			continue
		}
		if !workflowOnHasPullRequest(doc.On) {
			continue
		}
		if !workflowPermissionsContentsRead(doc.Permissions) {
			missing = append(missing, rel)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("pull_request workflows missing permissions contents: read: %s", strings.Join(missing, ", "))
	}
}

func TestWorkflowPermissionParsing(t *testing.T) {
	tests := []struct {
		name     string
		yml      string
		wantPR   bool
		wantPerm bool
	}{
		{
			name:     "mapping trigger",
			yml:      "on:\n  pull_request:\npermissions:\n  contents: read\n",
			wantPR:   true,
			wantPerm: true,
		},
		{
			name:     "inline trigger list",
			yml:      "on: [pull_request]\npermissions:\n  contents: read\n",
			wantPR:   true,
			wantPerm: true,
		},
		{
			name:     "commented permissions ignored",
			yml:      "on:\n  pull_request:\n# permissions:\n#   contents: read\n",
			wantPR:   true,
			wantPerm: false,
		},
		{
			name:     "nested script text is not a trigger",
			yml:      "on:\n  push:\njobs:\n  x:\n    steps:\n      - run: echo pull_request: permissions: contents: read\n",
			wantPR:   false,
			wantPerm: false,
		},
		{
			name:     "inline comment does not fake contents read",
			yml:      "on: [pull_request]\npermissions: write-all # contents: read\n",
			wantPR:   true,
			wantPerm: false,
		},
		{
			name:     "empty permissions mapping",
			yml:      "on: [pull_request]\npermissions: {}\n",
			wantPR:   true,
			wantPerm: false,
		},
		{
			name:     "write-all is not contents read",
			yml:      "on: [pull_request]\npermissions: write-all\n",
			wantPR:   true,
			wantPerm: false,
		},
		{
			name:     "quoted on and permissions keys",
			yml:      "\"on\": [pull_request]\n\"permissions\":\n  \"contents\": read\n",
			wantPR:   true,
			wantPerm: true,
		},
		{
			name:     "nested job permissions are not top-level",
			yml:      "on:\n  pull_request:\njobs:\n  x:\n    permissions:\n      contents: read\n",
			wantPR:   true,
			wantPerm: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowHasPullRequestTrigger(tc.yml); got != tc.wantPR {
				t.Fatalf("trigger = %v, want %v", got, tc.wantPR)
			}
			if got := workflowHasTopLevelContentsRead(tc.yml); got != tc.wantPerm {
				t.Fatalf("permissions = %v, want %v", got, tc.wantPerm)
			}
		})
	}
}

func TestVulncheckWindowsQuotesGobinPath(t *testing.T) {
	body := readRepoText(t, "Makefile")
	for _, want := range []string{
		`mkdir -p "$(CURDIR)/.cache/gobin"`,
		`GOBIN="$(CURDIR)/.cache/gobin"`,
		`"$(CURDIR)/.cache/gobin/govulncheck"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("vulncheck-windows missing quoted path %s", want)
		}
	}
}

type workflowFile struct {
	On          any `yaml:"on"`
	Permissions any `yaml:"permissions"`
}

func parseWorkflow(body string) (workflowFile, error) {
	var doc workflowFile
	err := yaml.Unmarshal([]byte(body), &doc)
	return doc, err
}

func workflowHasPullRequestTrigger(body string) bool {
	doc, err := parseWorkflow(body)
	if err != nil {
		return false
	}
	return workflowOnHasPullRequest(doc.On)
}

func workflowHasTopLevelContentsRead(body string) bool {
	doc, err := parseWorkflow(body)
	if err != nil {
		return false
	}
	return workflowPermissionsContentsRead(doc.Permissions)
}

func workflowOnHasPullRequest(on any) bool {
	switch v := on.(type) {
	case string:
		return v == "pull_request"
	case []any:
		for _, item := range v {
			if workflowOnHasPullRequest(item) {
				return true
			}
		}
	case map[string]any:
		_, ok := v["pull_request"]
		return ok
	}
	return false
}

func workflowPermissionsContentsRead(perm any) bool {
	m, ok := perm.(map[string]any)
	if !ok {
		return false
	}
	contents, ok := m["contents"]
	if !ok {
		return false
	}
	s, ok := contents.(string)
	return ok && s == "read"
}
