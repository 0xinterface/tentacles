package env

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestParseFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
		wantErr string // substring expected in the error, empty = no error
	}{
		{
			name:    "plain",
			content: "PATH=/usr/local/bin:/usr/bin:/bin\nHOME=/home/gha-runner\n",
			want:    map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/home/gha-runner"},
		},
		{
			name:    "double quoted",
			content: "PATH=\"/usr/local/bin:/usr/bin:/bin\"\n",
			want:    map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"},
		},
		{
			name:    "single quoted",
			content: "MISE_DATA_DIR='/home/gha-runner/.local/share/mise'\n",
			want:    map[string]string{"MISE_DATA_DIR": "/home/gha-runner/.local/share/mise"},
		},
		{
			name:    "comments and blanks",
			content: "# systemd EnvironmentFile\n\nPATH=/usr/bin\n   \n# another comment\nHOME=/home/x\n",
			want:    map[string]string{"PATH": "/usr/bin", "HOME": "/home/x"},
		},
		{
			name:    "spaces around equals",
			content: "PATH = /usr/bin\n",
			want:    map[string]string{"PATH": "/usr/bin"},
		},
		{
			name:    "empty value kept",
			content: "PATH=\n",
			want:    map[string]string{"PATH": ""},
		},
		{
			name:    "trailing text after value is not a comment",
			content: "PATH=/usr/bin # trailing\n",
			want:    map[string]string{"PATH": "/usr/bin # trailing"},
		},
		{
			name:    "malformed line reports line number",
			content: "PATH=/usr/bin\nNOTAVAR\nHOME=/home/x\n",
			wantErr: "2",
		},
		{
			name:    "malformed first line",
			content: "NOEQUALS\n",
			wantErr: "1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, tt.content)
			got, err := ParseFile(path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseFile: expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseFile error = %q, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseFile = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseFileMissing(t *testing.T) {
	if _, err := ParseFile(filepath.Join(t.TempDir(), "nope.env")); err == nil {
		t.Fatal("ParseFile: expected error for missing file")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want string // variable name in the error, empty = ok
	}{
		{"ok", map[string]string{"PATH": "/usr/bin", "HOME": "/home/x"}, ""},
		{"missing PATH", map[string]string{"HOME": "/home/x"}, "PATH"},
		{"missing HOME", map[string]string{"PATH": "/usr/bin"}, "HOME"},
		{"empty PATH", map[string]string{"PATH": "", "HOME": "/home/x"}, "PATH"},
		{"blank HOME", map[string]string{"PATH": "/usr/bin", "HOME": "  "}, "HOME"},
		{"empty vars", map[string]string{}, "PATH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.vars)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: expected error mentioning %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestApply(t *testing.T) {
	environ := []string{"PATH=/old-path", "HOME=/old-home", "OTHER=keep"}
	vars := map[string]string{
		"PATH":          "/new-path",
		"HOME":          "/new-home",
		"LANG":          "C.UTF-8",
		"MISE_DATA_DIR": "/home/gha-runner/.local/share/mise",
	}
	got := Apply(environ, vars)
	want := []string{
		"PATH=/new-path",
		"HOME=/new-home",
		"OTHER=keep",
		"LANG=C.UTF-8",
		"MISE_DATA_DIR=/home/gha-runner/.local/share/mise",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply = %v, want %v", got, want)
	}
}

func TestApplyNilVars(t *testing.T) {
	environ := []string{"PATH=/usr/bin"}
	got := Apply(environ, nil)
	if !reflect.DeepEqual(got, environ) {
		t.Fatalf("Apply(nil) = %v, want %v", got, environ)
	}
}
