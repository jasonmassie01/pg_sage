package startup

import (
	"slices"
	"strings"
	"testing"
)

// Unit tests for the ci-01 contract parsers: a parser that accepted
// anything would let TestToolchainPinsAgree pass on drifted pins.
// No concurrency or state surface: the parsers are pure functions.

const noToolchain = "go.mod has no toolchain line"

const testDigest = "3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a"

func TestParseToolchainInvalidInput(t *testing.T) {
	cases := map[string]struct {
		body    string
		want    string
		wantErr string
	}{
		"valid":           {body: "module m\n\ngo 1.26.0\n\ntoolchain go1.27.3\n", want: "1.27.3"},
		"crlf":            {body: "go 1.26.0\r\ntoolchain go1.27.3\r\n", want: "1.27.3"},
		"trailing remark": {body: "toolchain go1.27.3 // release\n", want: "1.27.3"},
		"missing":         {body: "module m\n\ngo 1.26.0\n", wantErr: noToolchain},
		"empty":           {body: "", wantErr: noToolchain},
		"commented out":   {body: "// toolchain go1.27.3\n", wantErr: noToolchain},
		"no patch":        {body: "toolchain go1.27\n", wantErr: "go.mod: malformed"},
		"default":         {body: "toolchain default\n", wantErr: "go.mod: malformed"},
		"no go prefix":    {body: "toolchain 1.27.3\n", wantErr: "go.mod: malformed"},
		"duplicate": {
			body:    "toolchain go1.27.3\ntoolchain go1.27.2\n",
			wantErr: "go.mod has 2 toolchain lines",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseToolchain("go.mod", tc.body)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseToolchain = %q, %v; want error containing %q",
						got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseToolchain = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestParseGoDirective(t *testing.T) {
	body := "module m\r\n\r\ngo 1.26.0\r\n\r\ntoolchain go1.27.3\r\n"
	got, err := parseGoDirective("go.mod", body)
	if err != nil || got != "1.26.0" {
		t.Fatalf("parseGoDirective = %q, %v; want 1.26.0", got, err)
	}
	if _, err := parseGoDirective("go.mod", "module m\n"); err == nil ||
		!strings.Contains(err.Error(), "go.mod has 0 go lines") {
		t.Fatalf("missing go line: err = %v, want one naming go.mod", err)
	}
	if _, err := parseGoDirective("go.mod", "go one.two\n"); err == nil ||
		!strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed go line: err = %v", err)
	}
}

func TestDockerStagesParsesFromLines(t *testing.T) {
	body := "# FROM golang:1.20 in a comment\r\n" +
		"FROM node:22-alpine AS frontend\r\n" +
		"from --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder\r\n" +
		"RUN echo FROM nowhere\r\n" +
		"FROM alpine:3.24@sha256:" + testDigest + "\r\n"
	want := []string{
		"node:22-alpine", "golang:1.27.3-alpine", "alpine:3.24@sha256:" + testDigest,
	}
	if got := dockerStages(body); !slices.Equal(got, want) {
		t.Fatalf("dockerStages = %q, want %q", got, want)
	}
	if got := dockerStages(""); len(got) != 0 {
		t.Fatalf("dockerStages(empty) = %q, want none", got)
	}
	ref := "alpine:3.24@sha256:" + testDigest
	if imageName(ref) != "alpine" || imageTag(ref) != "3.24" {
		t.Fatalf("imageName/imageTag(%s) = %q/%q", ref, imageName(ref), imageTag(ref))
	}
	if imageName("scratch") != "scratch" || imageTag("scratch") != "" {
		t.Fatal("scratch must parse as an untagged image")
	}
}

func TestCheckGolangImageBoundaries(t *testing.T) {
	cases := map[string]bool{
		"golang:1.27.3-alpine":                      true,
		"golang:1.27.3-alpine3.24":                  true,
		"golang:1.27.3":                             true,
		"golang:1.27.3-alpine@sha256:" + testDigest: true,
		"golang:1.27-alpine":                        false, // floating minor
		"golang:1.27.4-alpine":                      false, // patch ahead
		"golang:1.27.2-alpine":                      false, // patch behind
		"golang:1.26.10-alpine":                     false, // minor behind
		"golang:1.27.30-alpine":                     false, // prefix, not a match
		"golang:latest":                             false,
		"golang":                                    false,
	}
	for ref, ok := range cases {
		err := checkGolangImage(ref, "1.27.3")
		if (err == nil) != ok {
			t.Errorf("checkGolangImage(%s) = %v, want ok=%v", ref, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "go1.27.3") {
			t.Errorf("checkGolangImage(%s) error %q does not name the toolchain", ref, err)
		}
	}
}

func TestCheckCrossTagBoundaries(t *testing.T) {
	cases := map[string]bool{
		"v1.27.3":   true,
		"v1.27.3-1": true, // image rebuild of the same Go release
		"v1.27":     false,
		"v1.27.2":   false,
		"v1.27.30":  false,
		"v1.25.0":   false,
		"1.27.3":    false,
		"":          false,
	}
	for tag, ok := range cases {
		if err := checkCrossTag(tag, "1.27.3"); (err == nil) != ok {
			t.Errorf("checkCrossTag(%q) = %v, want ok=%v", tag, err, ok)
		}
	}
}

func TestCheckCrossRunBoundaries(t *testing.T) {
	const pinned = `docker run --rm -e GOTOOLCHAIN=go1.27.4 \`
	cases := []struct {
		name, run, tag string
		ok             bool
	}{
		{"exact tag, no GOTOOLCHAIN", `docker run --rm \`, "v1.27.4", true},
		{"older patch with GOTOOLCHAIN", pinned, "v1.27.3", true},
		{"older patch rebuild with GOTOOLCHAIN", pinned, "v1.27.3-1", true},
		{"older patch without GOTOOLCHAIN", `docker run --rm \`, "v1.27.3", false},
		{"older patch, GOTOOLCHAIN for another release", `docker run --rm -e GOTOOLCHAIN=go1.27.5 \`,
			"v1.27.3", false},
		{"older minor with GOTOOLCHAIN", pinned, "v1.26.9", false},
		{"newer patch with GOTOOLCHAIN", pinned, "v1.27.5", false},
		{"no v prefix", pinned, "1.27.3", false},
		{"empty tag", pinned, "", false},
	}
	for _, c := range cases {
		if err := checkCrossRun(c.run, c.tag, "1.27.4"); (err == nil) != c.ok {
			t.Errorf("%s: checkCrossRun(%q) = %v, want ok=%v", c.name, c.tag, err, c.ok)
		}
	}
}

func TestCheckDigestPinnedBoundaries(t *testing.T) {
	cases := map[string]bool{
		"alpine:3.24@sha256:" + testDigest:                  true,
		"alpine@sha256:" + testDigest:                       true,
		"alpine:3.24":                                       false,
		"alpine:3.24@sha256:" + testDigest[:63]:             false,
		"alpine:3.24@sha256:" + strings.ToUpper(testDigest): false,
		"alpine:3.24@sha256:" + testDigest + "0":            false,
		"alpine:3.24@sha512:" + testDigest + testDigest:     false,
		"": false,
	}
	for ref, ok := range cases {
		if err := checkDigestPinned(ref); (err == nil) != ok {
			t.Errorf("checkDigestPinned(%q) = %v, want ok=%v", ref, err, ok)
		}
	}
}

func TestCheckNodeMajorBoundaries(t *testing.T) {
	cases := map[string]bool{
		"22":         true,
		"22.21.1":    true,
		"v22":        true,
		"22-alpine":  true,
		"24-alpine":  true,
		"21.7.3":     false,
		"20":         false,
		"20-alpine":  false,
		"lts/*":      false,
		"node":       false,
		"":           false,
		"2":          false,
		"220-alpine": true, // a numbered major, not a prefix match on 22
	}
	for version, ok := range cases {
		err := checkNodeMajor("ci.yml", version)
		if (err == nil) != ok {
			t.Errorf("checkNodeMajor(%q) = %v, want ok=%v", version, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "ci.yml") {
			t.Errorf("checkNodeMajor(%q) error %q does not name its source", version, err)
		}
	}
}
