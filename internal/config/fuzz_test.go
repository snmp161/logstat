package config

import (
	"strings"
	"testing"
)

// FuzzParseAndValidate feeds arbitrary YAML through the whole config path. The
// contract is narrow but absolute: whatever an operator writes, logstat answers
// with a configuration or with an error — never with a panic. That matters more
// here than anywhere else, because the input is a file somebody edits by hand
// under time pressure, and because a panic in this path is a crash loop under
// Restart=always. One such panic (a metrics.path that http.ServeMux refused to
// parse) was found by hand; this is the machine looking for the rest.
func FuzzParseAndValidate(f *testing.F) {
	seeds := []string{
		"",
		"log_path: /var/log/app.log\n",
		"actions: [a, b]\ncase_sensitive: no\n",
		"flush_interval: 0\n",
		"redis:\n  port: 70000\n  ttl: -1\n",
		"logging:\n  output: file\n  file: \"\"\n",
		"reset:\n  schedule: \"*/5 * * * *\"\n",
		"metrics:\n  enabled: true\n  listen: \"127.0.0.1:9843\"\n  path: /metrics\n",
		"metrics:\n  path: \"/{env}\"\n",
		"metrics:\n  listen: \"[::1]:1\"\n",
		"actions: [\"\\u0000\", \"ПОЛУЧИТЬ\"]\ncase_sensitive: no\n",
		"actions: []\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse(data)
		if err != nil {
			if cfg != nil {
				t.Fatalf("Parse returned both a config and an error %v", err)
			}
			return
		}

		warnings, err := cfg.Validate()

		// Validate is also the gate in front of the exporter, so anything it
		// accepts has to be safe to hand to net/http. This is the assertion
		// that would have caught the ServeMux panic.
		if err == nil {
			if perr := cfg.Metrics.ValidatePath(); perr != nil {
				t.Fatalf("Validate accepted metrics.path %q that ValidatePath rejects: %v",
					cfg.Metrics.Path, perr)
			}
			if len(cfg.Actions) == 0 {
				t.Fatal("Validate accepted a config with no actions")
			}
			for _, a := range cfg.Actions {
				if a == "" {
					t.Fatal("Validate accepted an empty action")
				}
			}
		}

		// A second run must agree with the first. Validate normalises Actions in
		// place, so its warnings may shrink (the duplicates it complained about
		// are gone), but the verdict and the normalised list have to be a fixed
		// point — otherwise the daemon would run on something the validator
		// never saw, and no run would ever be the authoritative one.
		before := strings.Join(cfg.Actions, "\x00")
		warnings2, err2 := cfg.Validate()
		if (err == nil) != (err2 == nil) {
			t.Fatalf("Validate is not idempotent: first %v, second %v", err, err2)
		}
		if after := strings.Join(cfg.Actions, "\x00"); after != before {
			t.Fatalf("Validate rewrote actions on the second run: %q -> %q", before, after)
		}
		if len(warnings2) > len(warnings) {
			t.Fatalf("the second run invented warnings: %v then %v", warnings, warnings2)
		}
	})
}
