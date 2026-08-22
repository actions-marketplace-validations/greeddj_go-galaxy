package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSizes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []int
		ok   bool
	}{
		{name: "plain list", raw: "1,10,100", want: []int{1, 10, 100}, ok: true},
		{name: "spaces and trailing comma", raw: " 1 , 10 ,", want: []int{1, 10}, ok: true},
		{name: "zero refused", raw: "0"},
		{name: "negative refused", raw: "-3"},
		{name: "not a number", raw: "ten"},
		{name: "empty list", raw: " , "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSizes(tc.raw)

			if !tc.ok {
				if !errors.Is(err, errSizeInvalid) {
					t.Fatalf("parseSizes(%q) error = %v, want errSizeInvalid", tc.raw, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseSizes(%q) = %v", tc.raw, err)
			}

			if len(got) != len(tc.want) {
				t.Fatalf("parseSizes(%q) = %v, want %v", tc.raw, got, tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseSizes(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestParseScenariosRefusesWhatItCannotMeasure(t *testing.T) {
	got, err := parseScenarios("cold, warm")
	if err != nil {
		t.Fatalf("parseScenarios: %v", err)
	}

	if len(got) != 2 || got[0] != scenarioCold || got[1] != scenarioWarm {
		t.Fatalf("parseScenarios = %v, want [cold warm]", got)
	}

	// frozen is a scenario this binary does not implement. Accepting the name
	// and measuring nothing would be the worst of the three outcomes.
	if _, err := parseScenarios("cold,frozen"); !errors.Is(err, errScenarioUnknown) {
		t.Fatalf("parseScenarios(cold,frozen) error = %v, want errScenarioUnknown", err)
	}

	if _, err := parseScenarios(""); !errors.Is(err, errScenarioUnknown) {
		t.Fatalf("parseScenarios(empty) error = %v, want errScenarioUnknown", err)
	}
}

func TestCheckRequirementsNamesTheMissingFile(t *testing.T) {
	dir := t.TempDir()
	opts := options{requirementsDir: dir, sizes: []int{1, 10}}

	if err := os.WriteFile(filepath.Join(dir, "requirements-1.yml"), []byte("collections: []\n"), fileMode); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	err := opts.checkRequirements()
	if !errors.Is(err, errRequirementsMissing) {
		t.Fatalf("checkRequirements error = %v, want errRequirementsMissing", err)
	}

	// The point of validating up front is that the message says which file to
	// create, so a typo costs a second rather than a whole series.
	if want := "requirements-10.yml"; !strings.Contains(err.Error(), want) {
		t.Fatalf("checkRequirements error = %q, want it to name %q", err, want)
	}
}

func TestResolveBinaryRejectsWhatIsNotThere(t *testing.T) {
	_, err := resolveBinary(filepath.Join(t.TempDir(), "no-such-binary"))
	if !errors.Is(err, errBinaryMissing) {
		t.Fatalf("resolveBinary error = %v, want errBinaryMissing", err)
	}
}
