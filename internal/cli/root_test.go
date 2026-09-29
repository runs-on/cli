package cli

import (
	"bytes"
	"strings"
	"testing"
)

// --debug is one root flag, so every command accepts the spellings the
// commands used to define themselves, and debug output goes to stderr.
func TestDebugFlagOnEveryCommand(t *testing.T) {
	tests := []struct {
		args      []string
		wantDebug bool
	}{
		{args: []string{"logs"}},
		{args: []string{"logs", "-d"}, wantDebug: true},
		{args: []string{"stack", "logs", "-d"}, wantDebug: true},
		{args: []string{"connect", "--debug"}, wantDebug: true},
		{args: []string{"interrupt", "--debug"}, wantDebug: true},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			root := NewRootCmd(&Stack{})
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			cmd, flags, err := root.Find(tt.args)
			if err != nil {
				t.Fatalf("Find(%v): %v", tt.args, err)
			}
			if err := cmd.ParseFlags(flags); err != nil {
				t.Fatalf("ParseFlags(%v): %v", flags, err)
			}

			debugLogger(cmd).Print("debug line")

			want := ""
			if tt.wantDebug {
				want = "debug line\n"
			}
			if stderr.String() != want || stdout.Len() != 0 {
				t.Errorf("stdout = %q, stderr = %q; want only %q on stderr", stdout.String(), stderr.String(), want)
			}
		})
	}
}
