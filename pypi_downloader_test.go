package pycage

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
)

func TestParsePyPIRequirement(t *testing.T) {
	tests := []struct {
		input   string
		name    string
		version string
		ok      bool
	}{
		{"six==1.17.0", "six", "1.17.0", true},
		{"requests[security]>=2; python_version >= '3.10'", "requests", "", true},
		{"urllib3<3,>=2", "urllib3", "", true},
		{"httpcore==1.*", "httpcore", "", true},
		{"/packages/local.whl", "", "", false},
		{"bad name", "", "", false},
		{"six==1.17.0,<2", "", "", false},
	}
	for _, test := range tests {
		name, version, ok := parsePyPIRequirement(test.input)
		if name != test.name || version != test.version || ok != test.ok {
			t.Errorf("parsePyPIRequirement(%q) = (%q, %q, %v), want (%q, %q, %v)",
				test.input, name, version, ok, test.name, test.version, test.ok)
		}
	}
}

func TestStripWheelEntryPoints(t *testing.T) {
	var input bytes.Buffer
	writer := zip.NewWriter(&input)
	for name, contents := range map[string]string{
		"demo/__init__.py":                    "value = 42\n",
		"demo-1.0.dist-info/entry_points.txt": "[console_scripts]\ndemo=demo:main\n",
		"demo-1.0.dist-info/METADATA":         "Name: demo\nVersion: 1.0\n",
	} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(file, contents)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	stripped, err := stripWheelEntryPoints(input.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(stripped), int64(len(stripped)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if file.Name == "demo-1.0.dist-info/entry_points.txt" {
			t.Fatal("console entry points were not stripped")
		}
	}
	if len(reader.File) != 2 {
		t.Fatalf("wheel contains %d files, want 2", len(reader.File))
	}
}
