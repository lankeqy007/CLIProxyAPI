package config

import "testing"

func TestApplyRuntimeEnvOverridesHostAndPort(t *testing.T) {
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PORT", "10000")

	cfg := &Config{
		Host: "127.0.0.1",
		Port: 8317,
	}

	if err := ApplyRuntimeEnv(cfg); err != nil {
		t.Fatalf("ApplyRuntimeEnv returned error: %v", err)
	}

	if cfg.Host != "0.0.0.0" {
		t.Fatalf("unexpected host: got %q want %q", cfg.Host, "0.0.0.0")
	}
	if cfg.Port != 10000 {
		t.Fatalf("unexpected port: got %d want %d", cfg.Port, 10000)
	}
}

func TestApplyRuntimeEnvSupportsAliases(t *testing.T) {
	t.Setenv("APP_HOST", "127.0.0.1")
	t.Setenv("APP_PORT", "9000")

	cfg := &Config{}
	if err := ApplyRuntimeEnv(cfg); err != nil {
		t.Fatalf("ApplyRuntimeEnv returned error: %v", err)
	}

	if cfg.Host != "127.0.0.1" {
		t.Fatalf("unexpected host: got %q want %q", cfg.Host, "127.0.0.1")
	}
	if cfg.Port != 9000 {
		t.Fatalf("unexpected port: got %d want %d", cfg.Port, 9000)
	}
}

func TestApplyRuntimeEnvRejectsInvalidPort(t *testing.T) {
	t.Setenv("PORT", "bad-port")

	cfg := &Config{}
	if err := ApplyRuntimeEnv(cfg); err == nil {
		t.Fatal("expected invalid port error, got nil")
	}
}

func TestApplyRuntimeEnvSetsClientAPIKeysFromEnv(t *testing.T) {
	t.Run("single key", func(t *testing.T) {
		t.Setenv("CLIENT_API_KEY", "k1")
		cfg := &Config{}
		if err := ApplyRuntimeEnv(cfg); err != nil {
			t.Fatalf("ApplyRuntimeEnv returned error: %v", err)
		}
		if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "k1" {
			t.Fatalf("unexpected api keys: got %#v want %#v", cfg.APIKeys, []string{"k1"})
		}
	})

	t.Run("multiple keys", func(t *testing.T) {
		t.Setenv("CLIENT_API_KEYS", "k1, k2,  ,k3")
		cfg := &Config{}
		if err := ApplyRuntimeEnv(cfg); err != nil {
			t.Fatalf("ApplyRuntimeEnv returned error: %v", err)
		}
		want := []string{"k1", "k2", "k3"}
		if len(cfg.APIKeys) != len(want) {
			t.Fatalf("unexpected api keys: got %#v want %#v", cfg.APIKeys, want)
		}
		for i := range want {
			if cfg.APIKeys[i] != want[i] {
				t.Fatalf("unexpected api key at %d: got %q want %q (all=%#v)", i, cfg.APIKeys[i], want[i], cfg.APIKeys)
			}
		}
	})
}

func TestIsCloudDeployEnv(t *testing.T) {
	t.Run("explicit deploy flag", func(t *testing.T) {
		t.Setenv("DEPLOY", "cloud")
		if !IsCloudDeployEnv() {
			t.Fatal("expected cloud deploy detection")
		}
	})

	t.Run("render runtime", func(t *testing.T) {
		t.Setenv("RENDER", "true")
		if !IsCloudDeployEnv() {
			t.Fatal("expected Render runtime detection")
		}
	})

	t.Run("hugging face runtime", func(t *testing.T) {
		t.Setenv("SPACE_ID", "user/demo")
		if !IsCloudDeployEnv() {
			t.Fatal("expected Hugging Face runtime detection")
		}
	})
}
