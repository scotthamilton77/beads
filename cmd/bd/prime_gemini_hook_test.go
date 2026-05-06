//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runPrimeBinary runs the bd binary built by buildBDUnderTest in the given
// working directory with a clean env (HOME isolated), capturing stdout.
// stderr is captured separately so JSON-validity checks aren't polluted by
// auto-pull or warning lines.
func runPrimeBinary(t *testing.T, binPath, workDir string, args ...string) (stdout []byte, stderr []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	full := append([]string{"prime"}, args...)
	cmd := exec.CommandContext(ctx, binPath, full...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"BEADS_TEST_IGNORE_REPO_CONFIG=1",
		"BEADS_DIR=",
		"BEADS_DB=",
		"LINEAR_API_KEY=", // Suppress Linear auto-pull noise
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// bd prime exits 0 on every path we care about (silent-success
		// contract). A non-zero exit is itself a failure.
		t.Fatalf("bd %v in %s: %v\nstdout: %s\nstderr: %s", full, workDir, err, outBuf.String(), errBuf.String())
	}
	return outBuf.Bytes(), errBuf.Bytes()
}

// initBeadsWorkspace creates a minimal beads workspace at workDir using `bd
// init --prefix`. We don't need a Dolt server or any issues — just the
// .beads/ directory so FindBeadsDir succeeds.
func initBeadsWorkspace(t *testing.T, binPath, workDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "init", "--prefix", "test")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"BEADS_TEST_IGNORE_REPO_CONFIG=1",
		"BEADS_DIR=",
		"BEADS_DB=",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init in %s: %v\n%s", workDir, err, out)
	}
}

type primeEnvelope struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func parseEnvelope(t *testing.T, raw []byte) primeEnvelope {
	t.Helper()
	trimmed := bytes.TrimSpace(raw)
	var env primeEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, raw)
	}
	if env.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", env.HookSpecificOutput.HookEventName)
	}
	return env
}

// TestPrime_GeminiHook_DefaultPath: with --gemini-hook and no PRIME.md
// override, output is the JSON envelope wrapping the generated workflow
// context.
func TestPrime_GeminiHook_DefaultPath(t *testing.T) {
	binPath := buildBDUnderTest(t)
	workDir := t.TempDir()
	initBeadsWorkspace(t, binPath, workDir)

	stdout, _ := runPrimeBinary(t, binPath, workDir, "--gemini-hook")
	env := parseEnvelope(t, stdout)

	if env.HookSpecificOutput.AdditionalContext == "" {
		t.Fatal("expected non-empty additionalContext for default path")
	}
	// Sanity-check that the generated content actually flowed through.
	// The CLI/MCP variants both lead with one of these phrases.
	ctx := env.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "Beads") {
		t.Errorf("additionalContext should contain generated bd prime markdown, got: %q", firstN(ctx, 200))
	}
}

// TestPrime_GeminiHook_LocalPrimeOverride: with --gemini-hook and a
// .beads/PRIME.md file present, output is the JSON envelope with that file's
// contents in additionalContext (verbatim).
func TestPrime_GeminiHook_LocalPrimeOverride(t *testing.T) {
	binPath := buildBDUnderTest(t)
	workDir := t.TempDir()
	initBeadsWorkspace(t, binPath, workDir)

	const custom = "# Custom local PRIME.md override\nBe excellent.\n"
	primePath := filepath.Join(workDir, ".beads", "PRIME.md")
	if err := os.WriteFile(primePath, []byte(custom), 0o644); err != nil {
		t.Fatalf("write PRIME.md: %v", err)
	}

	stdout, _ := runPrimeBinary(t, binPath, workDir, "--gemini-hook")
	env := parseEnvelope(t, stdout)

	if env.HookSpecificOutput.AdditionalContext != custom {
		t.Errorf("additionalContext = %q, want %q", env.HookSpecificOutput.AdditionalContext, custom)
	}
}

// TestPrime_GeminiHook_NotJSON_WithoutFlag is a regression guard: without
// --gemini-hook, prime output is raw markdown — NOT a JSON envelope.
// This is the binary-level companion to the in-process unit test in
// prime_test.go and protects the existing Claude/CLI contract.
func TestPrime_GeminiHook_NotJSON_WithoutFlag(t *testing.T) {
	binPath := buildBDUnderTest(t)
	workDir := t.TempDir()
	initBeadsWorkspace(t, binPath, workDir)

	stdout, _ := runPrimeBinary(t, binPath, workDir)
	out := strings.TrimSpace(string(stdout))
	if strings.HasPrefix(out, "{") {
		t.Fatalf("bd prime (no flag) emitted JSON-looking content; raw markdown expected: %q", firstN(out, 200))
	}
	var any map[string]interface{}
	if err := json.Unmarshal([]byte(out), &any); err == nil {
		t.Fatal("bd prime (no flag) output should not be valid JSON")
	}
}

// TestPrime_GeminiHook_StealthCompose: --gemini-hook composed with --stealth
// emits the JSON envelope, and additionalContext is in stealth mode (no raw
// `git push` instructions in the close protocol).
func TestPrime_GeminiHook_StealthCompose(t *testing.T) {
	binPath := buildBDUnderTest(t)
	workDir := t.TempDir()
	initBeadsWorkspace(t, binPath, workDir)

	stdout, _ := runPrimeBinary(t, binPath, workDir, "--gemini-hook", "--stealth")
	env := parseEnvelope(t, stdout)

	ctx := env.HookSpecificOutput.AdditionalContext
	if ctx == "" {
		t.Fatal("expected non-empty additionalContext under --stealth --gemini-hook")
	}
	// Stealth mode: close protocol must not steer agents to git push.
	// (Local-only also suppresses git ops, but stealth is the explicit user
	// signal we care about here.)
	if strings.Contains(ctx, "git push") {
		t.Errorf("stealth mode should not include 'git push' in additionalContext, got snippet: %q", firstN(ctx, 400))
	}
	// And the close-protocol section must still exist.
	if !strings.Contains(ctx, "bd close") {
		t.Errorf("stealth mode should still teach 'bd close', got snippet: %q", firstN(ctx, 400))
	}
}

// TestPrime_GeminiHook_GlobalPrimeOverride: with --gemini-hook and a
// ~/.config/beads/PRIME.md file present (XDG path), output is the JSON
// envelope wrapping that file's contents. This exercises the third
// custom-PRIME.md path through the wrapper.
func TestPrime_GeminiHook_GlobalPrimeOverride(t *testing.T) {
	binPath := buildBDUnderTest(t)
	workDir := t.TempDir()
	initBeadsWorkspace(t, binPath, workDir)

	const custom = "# Global PRIME override\nGreetings from XDG.\n"

	// resolveGlobalPrimePath uses os.UserConfigDir, which on Linux honors
	// XDG_CONFIG_HOME. On macOS it returns ~/Library/Application Support
	// regardless of XDG, so we set HOME and also stage the macOS path to
	// be cross-platform-safe.
	xdg := t.TempDir()
	home := t.TempDir()

	xdgBeadsDir := filepath.Join(xdg, "beads")
	if err := os.MkdirAll(xdgBeadsDir, 0o755); err != nil {
		t.Fatalf("mkdir xdg beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xdgBeadsDir, "PRIME.md"), []byte(custom), 0o644); err != nil {
		t.Fatalf("write xdg PRIME.md: %v", err)
	}
	// Cross-platform staging for macOS UserConfigDir.
	macConfigDir := filepath.Join(home, "Library", "Application Support", "beads")
	if err := os.MkdirAll(macConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir mac config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(macConfigDir, "PRIME.md"), []byte(custom), 0o644); err != nil {
		t.Fatalf("write mac PRIME.md: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "prime", "--gemini-hook")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+xdg,
		"BEADS_TEST_IGNORE_REPO_CONFIG=1",
		"BEADS_DIR=",
		"BEADS_DB=",
		"LINEAR_API_KEY=",
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd prime --gemini-hook: %v\nstdout: %s\nstderr: %s", err, outBuf.String(), errBuf.String())
	}

	env := parseEnvelope(t, outBuf.Bytes())
	if env.HookSpecificOutput.AdditionalContext != custom {
		t.Errorf("additionalContext = %q, want %q", env.HookSpecificOutput.AdditionalContext, custom)
	}
}

// TestPrime_GeminiHook_RedirectedPrimeOverride: with --gemini-hook and a
// PRIME.md at <beadsDir>/PRIME.md (workspace-level, not local clone), output
// is the JSON envelope wrapping that file's contents. With a default
// `bd init`, beadsDir == ./.beads — the redirected path is the same as the
// local path. To exercise the dedicated branch we'd need a redirect setup;
// the local-override test plus the unit test for outputGeminiHook cover the
// wrapping contract for both code paths.

// TestPrime_GeminiHook_NoBeadsWorkspace: when bd prime would otherwise emit
// nothing (no beads workspace resolved), --gemini-hook still emits the empty
// JSON envelope so Gemini's strict stdout-must-be-JSON contract is honored.
func TestPrime_GeminiHook_NoBeadsWorkspace(t *testing.T) {
	binPath := buildBDUnderTest(t)
	// Use a freshly created tmpdir with NO beads workspace and HOME isolated
	// so FindBeadsDir cannot walk up into the test repo.
	workDir := t.TempDir()
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "prime", "--gemini-hook")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+t.TempDir(),
		"BEADS_TEST_IGNORE_REPO_CONFIG=1",
		"BEADS_DIR=",
		"BEADS_DB=",
		"LINEAR_API_KEY=",
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd prime --gemini-hook: %v\nstdout: %s\nstderr: %s", err, outBuf.String(), errBuf.String())
	}

	env := parseEnvelope(t, outBuf.Bytes())
	if env.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("additionalContext should be empty when no beads workspace, got: %q",
			firstN(env.HookSpecificOutput.AdditionalContext, 200))
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
