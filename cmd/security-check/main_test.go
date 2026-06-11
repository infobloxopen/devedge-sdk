package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/infobloxopen/devedge-sdk/seccheck"
)

// syntheticFDS returns a FileDescriptorSet containing one service ("WidgetService")
// with one method ("GetWidget") that has NO authz annotation.
func syntheticFDS() *descriptorpb.FileDescriptorSet {
	syntax := "proto3"
	name := "test/widgets.proto"
	pkg := "test.widgets.v1"
	svcName := "WidgetService"
	methodName := "GetWidget"
	inputType := ".test.widgets.v1.GetWidgetRequest"
	outputType := ".test.widgets.v1.GetWidgetResponse"

	// We need stub message descriptors so the method's input/output types resolve.
	reqMsg := "GetWidgetRequest"
	resMsg := "GetWidgetResponse"

	return &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    &name,
				Package: &pkg,
				Syntax:  &syntax,
				MessageType: []*descriptorpb.DescriptorProto{
					{Name: &reqMsg},
					{Name: &resMsg},
				},
				Service: []*descriptorpb.ServiceDescriptorProto{
					{
						Name: &svcName,
						Method: []*descriptorpb.MethodDescriptorProto{
							{
								Name:       &methodName,
								InputType:  &inputType,
								OutputType: &outputType,
								// Deliberately NO options — simulates a method with no authz annotation.
							},
						},
					},
				},
			},
		},
	}
}

// TestCheckDescriptor_NoAnnotation verifies that a method with no authz
// annotation and no rules file produces a single Error finding.
func TestCheckDescriptor_NoAnnotation(t *testing.T) {
	fds := syntheticFDS()
	findings := checkDescriptor(fds, nil)

	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Severity != seccheck.Error {
		t.Errorf("expected Severity=Error, got %v", f.Severity)
	}
	wantMethod := "/test.widgets.v1.WidgetService/GetWidget"
	if f.Method != wantMethod {
		t.Errorf("expected Method=%q, got %q", wantMethod, f.Method)
	}
	if !strings.Contains(f.Message, "no authz annotation") {
		t.Errorf("unexpected message: %q", f.Message)
	}
}

// TestRun_ExitCode1_NoAnnotation verifies that run() returns exit code 1 when
// the descriptor contains an unannotated method.
func TestRun_ExitCode1_NoAnnotation(t *testing.T) {
	// Write the FileDescriptorSet to a temp file so run() can read it.
	fds := syntheticFDS()
	raw, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshalling synthetic FDS: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "test-*.binpb")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.Write(raw); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	f.Close()

	// Capture stdout so we can inspect the printed findings.
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	code := run([]string{"--descriptor", f.Name()})

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}

	wantSubstr := "[error] /test.widgets.v1.WidgetService/GetWidget"
	if !strings.Contains(output, wantSubstr) {
		t.Errorf("expected output to contain %q\ngot: %s", wantSubstr, output)
	}
	fmt.Printf("captured output:\n%s", output)
}

// TestRun_NoDescriptorFlag verifies that run() returns 1 when --descriptor is missing.
func TestRun_NoDescriptorFlag(t *testing.T) {
	if code := run([]string{}); code != 1 {
		t.Errorf("expected exit code 1 without --descriptor, got %d", code)
	}
}
