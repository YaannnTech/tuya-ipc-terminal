package cmd

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func newTestRoot(runErr error) (*cobra.Command, *bytes.Buffer) {
	root := &cobra.Command{Use: "root", PersistentPreRunE: silenceUsage}
	root.AddCommand(&cobra.Command{
		Use:  "run",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runErr },
	})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	return root, out
}

func TestSilenceUsage(t *testing.T) {
	root, out := newTestRoot(errors.New("boom"))
	root.SetArgs([]string{"run", "x"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error")
	}
	if bytes.Contains(out.Bytes(), []byte("Usage:")) {
		t.Errorf("runtime error printed usage: %s", out)
	}

	root, out = newTestRoot(nil)
	root.SetArgs([]string{"run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected argument error")
	}
	if !bytes.Contains(out.Bytes(), []byte("Usage:")) {
		t.Errorf("argument error did not print usage: %s", out)
	}
}
