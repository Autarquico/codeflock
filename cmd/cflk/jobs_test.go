package main

import (
	"context"
	"os"
	"testing"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
)

func isolated(t *testing.T) *localJobs {
	t.Helper()
	t.Setenv(stateDirVar, t.TempDir())
	return newLocalJobs("/cflk")
}

func TestAnEmptyStateListsNothing(t *testing.T) {
	jobs, err := isolated(t).List(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Errorf("jobs = %v, %v", jobs, err)
	}
}

func TestACommandOnWhatIsNotAJobOrDoesNotExistSaysSo(t *testing.T) {
	l := isolated(t)
	if _, err := l.Show(context.Background(), "../x"); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("a bad id: %v", err)
	}
	if _, err := l.Show(context.Background(), "job-aaaaaaaaaa"); codeOf(err) != domain.CodeNotFound {
		t.Errorf("a missing Job: %v", err)
	}
}

func TestRunRefusesAnUnknownHarnessAndAnotherPeerBeforeTouchingAnything(t *testing.T) {
	l := isolated(t)
	if _, err := l.Run(context.Background(), cli.RunRequest{Harness: "gpt", Prompt: "x"}); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("an unknown harness: %v", err)
	}
	if _, err := l.Run(context.Background(), cli.RunRequest{On: "mac-mini", Prompt: "x"}); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("another peer: %v", err)
	}
	if jobs, _ := l.List(context.Background()); len(jobs) != 0 {
		t.Errorf("a refused run left Jobs: %v", jobs)
	}
}

func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f) {
		t.Error("/dev/null is a character device, not a terminal")
	}
}
