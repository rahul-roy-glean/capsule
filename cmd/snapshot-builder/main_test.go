package main

import "testing"

func TestValidateBuildModeAllowsColdBootWithoutChunked(t *testing.T) {
	if err := validateBuildMode(false, false); err != nil {
		t.Fatalf("validateBuildMode(false, false) returned error: %v", err)
	}
}

func TestValidateBuildModeRejectsIncrementalWithoutChunked(t *testing.T) {
	err := validateBuildMode(true, false)
	if err == nil {
		t.Fatal("validateBuildMode(true, false) returned nil, want error")
	}
}

func TestShouldPublishCurrentPointerRejectsMissingUpload(t *testing.T) {
	err := shouldPublishCurrentPointer(false)
	if err == nil {
		t.Fatal("shouldPublishCurrentPointer(false) returned nil, want error")
	}
}

func TestShouldPublishCurrentPointerAllowsUploadedSnapshot(t *testing.T) {
	if err := shouldPublishCurrentPointer(true); err != nil {
		t.Fatalf("shouldPublishCurrentPointer(true) returned error: %v", err)
	}
}
