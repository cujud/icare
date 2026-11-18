package main

import (
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	values, err := parseDotEnv(strings.NewReader(`
# Firebase settings
 FIREBASE_URL = "https://example.test/db/"
export FIREBASE_TOKEN='token=value'
`))
	if err != nil {
		t.Fatal(err)
	}
	if values["FIREBASE_URL"] != "https://example.test/db/" {
		t.Errorf("FIREBASE_URL = %q", values["FIREBASE_URL"])
	}
	if values["FIREBASE_TOKEN"] != "token=value" {
		t.Errorf("FIREBASE_TOKEN = %q", values["FIREBASE_TOKEN"])
	}
}

func TestParseDotEnvRejectsMalformedVariable(t *testing.T) {
	if _, err := parseDotEnv(strings.NewReader("NOT A NAME=value\n")); err == nil {
		t.Fatal("parseDotEnv accepted an invalid environment variable name")
	}
}
