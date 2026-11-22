package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func baseEnv() map[string]string {
	return map[string]string{
		"MUSTER_URL":          "http://muster.muster-tenant.svc.cluster.local.:8105",
		"MUSTER_PLUGIN_TOKEN": "mpl_s3cretT0ken",
		"MUSTER_ORIGIN":       "https://muster.example.test",
		"CONTACTS_PUBLIC_URL": "https://contacts-muster.example.test",
		"CONTACTS_API_TOKEN":  "an-api-token-of-at-least-32-bytes-0123",
	}
}

func load(env map[string]string) (Config, error) {
	return LoadConfig(func(k string) string { return env[k] })
}

func TestTheExampleConfigLoads(t *testing.T) {
	env := baseEnv()
	env["CONTACTS_MEETINGS_APP_ID"] = "12"
	c, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.MeetingsAppID != 12 || c.Listen != ":8080" || c.InternalListen != ":8081" || c.StateDir != "./state" {
		t.Fatalf("%+v", c)
	}
}

// 🔴 The API token: required, at least 32 bytes, never a muster token; and
// no refusal echoes a token's value.
func TestTheAPITokenIsCheckedAtBoot(t *testing.T) {
	for name, c := range map[string]struct {
		key, value, want string
	}{
		"unset":                     {"CONTACTS_API_TOKEN", "", "CONTACTS_API_TOKEN is not set"},
		"short":                     {"CONTACTS_API_TOKEN", "only-31-bytes-of-token-material", "shorter than 32 bytes"},
		"the example's placeholder": {"CONTACTS_API_TOKEN", "replace-me", "the .env.example placeholder"},
		"the old placeholder":       {"CONTACTS_API_TOKEN", "replace-me-with-openssl-rand-base64-32", "the .env.example placeholder"},
		"32 times a":                {"CONTACTS_API_TOKEN", strings.Repeat("a", 32), "fewer than 16 distinct characters"},
		"the muster token":          {"CONTACTS_API_TOKEN", "mpl_s3cretT0ken", "is a muster plugin token"},
		"another muster token":      {"CONTACTS_API_TOKEN", "mpl_" + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG", "is a muster plugin token"},
		"public url is muster's":    {"CONTACTS_PUBLIC_URL", "https://muster.example.test", "must be its own origin"},
		"a list of muster tokens":   {"MUSTER_PLUGIN_TOKEN", "mpl_a,mpl_b", "holds a comma"},
		"meetings id not a number":  {"CONTACTS_MEETINGS_APP_ID", "twelve", "CONTACTS_MEETINGS_APP_ID"},
		"origin with a wildcard":    {"MUSTER_ORIGIN", "https://*.example.test", "one host"},
		"muster over plain http":    {"MUSTER_URL", "http://muster.example.test", "must be https://"},
	} {
		env := baseEnv()
		env[c.key] = c.value
		_, err := load(env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error holding %q", name, err, c.want)
			continue
		}
		if c.value != "" && strings.Contains(c.key, "TOKEN") && strings.Contains(err.Error(), c.value) {
			t.Errorf("%s: the refusal echoes the token: %v", name, err)
		}
	}
	env := baseEnv()
	// The boundaries: exactly 32 bytes with exactly 16 distinct ones.
	env["CONTACTS_API_TOKEN"] = "0123456789abcdef0123456789abcdef"
	if _, err := load(env); err != nil {
		t.Fatalf("a 32-byte token of 16 distinct bytes: %v (the boundary is accepted)", err)
	}
	// And the example file's own value is refused.
	example, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CONTACTS_API_TOKEN=(.*)$`).FindSubmatch(example)
	if m == nil {
		t.Fatal(".env.example sets no CONTACTS_API_TOKEN")
	}
	env["CONTACTS_API_TOKEN"] = string(m[1])
	if _, err := load(env); err == nil {
		t.Fatalf(".env.example's CONTACTS_API_TOKEN %q boots", m[1])
	}
}
