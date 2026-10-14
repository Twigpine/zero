package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestModelRPMRoundTrip(t *testing.T) {
	var c FileConfig
	if e := json.Unmarshal([]byte(`{"modelRPM":{" OPENAI:GPT-4.1 ":2,"custom":0},"future":true}`), &c); e != nil {
		t.Fatal(e)
	}
	want := map[string]int{"gpt-4.1": 2, "custom": 0}
	if !reflect.DeepEqual(c.ModelRPM, want) {
		t.Fatal(c.ModelRPM)
	}
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	var d FileConfig
	if e = json.Unmarshal(b, &d); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(d.ModelRPM, want) || string(d.Extra["future"]) != "true" {
		t.Fatalf("round trip=%s", b)
	}
}
func TestModelRPMInvalid(t *testing.T) {
	for _, s := range []string{`{"modelRPM":{"":1}}`, `{"modelRPM":{"a":-1}}`, `{"modelRPM":{"a":1.5}}`, `{"modelRPM":{"a":"2"}}`, `{"modelRPM":{"gpt-4.1":1,"openai:gpt-4.1":2}}`} {
		var c FileConfig
		if e := json.Unmarshal([]byte(s), &c); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestModelRPMProjectCanOnlyTighten(t *testing.T) {
	user := writeConfig(t, `{"modelRPM":{"gpt-4.1":2},"activeProvider":"test","providers":[{"name":"test","provider":"openai","model":"gpt-4.1"}]}`)
	for _, n := range []string{"0", "1", "5"} {
		project := writeConfig(t, `{"modelRPM":{"openai:gpt-4.1":`+n+`}}`)
		c, e := Resolve(ResolveOptions{UserConfigPath: user, ProjectConfigPath: project, Env: map[string]string{}})
		if e != nil {
			t.Fatal(e)
		}
		want := 2
		if n == "1" {
			want = 1
		}
		if c.ModelRPM["gpt-4.1"] != want {
			t.Fatal(c.ModelRPM)
		}
	}
}
