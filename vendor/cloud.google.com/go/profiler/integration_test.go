// Copyright 2017 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package profiler

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"cloud.google.com/go/profiler/proftest"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
)

const (
	cloudScope        = "https://www.googleapis.com/auth/cloud-platform"
	benchFinishString = "busybench finished profiling"
	errorString       = "failed to set up or run the benchmark"
)

const startupTemplate = `
#! /bin/bash

# Signal any unexpected error.
trap 'echo "{{.ErrorString}}"' ERR

(
# Shut down the VM in 5 minutes after this script exits
# to stop accounting the VM for billing and cores quota.
trap "sleep 300 && poweroff" EXIT

retry() {
  for i in {1..3}; do
    "${@}" && return 0
  done
  return 1
}

# Fail on any error.
set -eo pipefail

# Display commands being run.
set -x

# Suppress debconf warnings to minimize noise in logs
export DEBIAN_FRONTEND="noninteractive"

# Building go from master will fail without $HOME set.
# Set $HOME becasue it is not automatically set when this script runs.
# If $HOME is unset, $GOCACHE must be set for Go 1.12+
cd /root
export HOME=$PWD

# Install git
retry apt-get update >/dev/null
retry apt-get -y -q install git >/dev/null

# Set $GOPATH
export GOPATH="$HOME/go"

export GOCLOUD_HOME=$GOPATH/src/cloud.google.com/go
mkdir -p $GOCLOUD_HOME

# Install gcc, needed to install go master
if [ "{{.GoVersion}}" = "master" ]
then
retry apt-get -y -q install gcc >/dev/null
fi

# Install desired Go version
mkdir -p /tmp/bin
retry curl -sL -o /tmp/bin/gimme https://raw.githubusercontent.com/travis-ci/gimme/master/gimme
chmod +x /tmp/bin/gimme
export PATH=$PATH:/tmp/bin

retry gimme {{.GoVersion}} > out.gimme
eval "$(cat out.gimme)"

# Install agent
retry git clone https://code.googlesource.com/gocloud $GOCLOUD_HOME >/dev/null
cd $GOCLOUD_HOME
retry git fetch origin {{.Commit}}
git reset --hard {{.Commit}}

cd $GOCLOUD_HOME/profiler/busybench
retry go get >/dev/null

# Run benchmark with agent
go run busybench.go --service="{{.Service}}" --mutex_profiling="{{.MutexProfiling}}"

# Write output to serial port 2 with timestamp.
) 2>&1 | while read line; do echo "$(date): ${line}"; done >/dev/ttyS1
`

type goGCETestCase struct {
	proftest.InstanceConfig
	name             string
	goVersion        string
	mutexProfiling   bool
	wantProfileTypes []string
}

func (tc *goGCETestCase) initializeStartupScript(template *template.Template, commit string) error {
	var buf bytes.Buffer
	err := template.Execute(&buf,
		struct {
			Service        string
			GoVersion      string
			Commit         string
			ErrorString    string
			MutexProfiling bool
		}{
			Service:        tc.name,
			GoVersion:      tc.goVersion,
			Commit:         commit,
			ErrorString:    errorString,
			MutexProfiling: tc.mutexProfiling,
		})
	if err != nil {
		return fmt.Errorf("failed to render startup script for %s: %v", tc.name, err)
	}
	tc.StartupScript = buf.String()
	return nil
}

func TestAgentIntegration(t *testing.T) {
	// Testing against master requires building go code and may take up to 10 minutes.
	// Allow this test to run in parallel with other top level tests to avoid timeouts.
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping profiler integration test in short mode")
	}

	projectID := os.Getenv("GCLOUD_TESTS_GOLANG_PROJECT_ID")
	if projectID == "" {
		t.Skip("skipping profiler integration test when GCLOUD_TESTS_GOLANG_PROJECT_ID variable is not set")
	}

	zone := os.Getenv("GCLOUD_TESTS_GOLANG_PROFILER_ZONE")
	if zone == "" {
		t.Fatalf("GCLOUD_TESTS_GOLANG_PROFILER_ZONE environment variable must be set when integration test is requested")
	}

	// Figure out the Git commit of the current directory. The source checkout in
	// the test VM will run in the same commit. Note that any local changes to
	// the profiler agent won't be tested in the integration test. This flow only
	// works with code that has been committed and pushed to the public repo
	// (either to master or to a branch).
	output, err := exec.Command("git", "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("failed to gather the Git revision of the current source: %v", err)
	}
	commit := strings.Trim(string(output), "\n")
	t.Logf("using Git commit %q for the profiler integration test", commit)

	pst, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("failed to initiate PST location: %v", err)
	}
	runID := strings.Replace(time.Now().In(pst).Format("2006-01-02-15-04-05.000000-0700"), ".", "-", -1)

	ctx := context.Background()

	client, err := google.DefaultClient(ctx, cloudScope)
	if err != nil {
		t.Fatalf("failed to get default client: %v", err)
	}

	computeService, err := compute.New(client)
	if err != nil {
		t.Fatalf("failed to initialize compute service: %v", err)
	}

	template, err := template.New("startupScript").Parse(startupTemplate)
	if err != nil {
		t.Fatalf("failed to parse startup script template: %v", err)
	}

	tr := proftest.TestRunner{
		Client: client,
	}

	gceTr := proftest.GCETestRunner{
		TestRunner:     tr,
		ComputeService: computeService,
	}

	testcases := []goGCETestCase{
		{
			InstanceConfig: proftest.InstanceConfig{
				ProjectID:   projectID,
				Zone:        zone,
				Name:        fmt.Sprintf("profiler-test-gomaster-%s", runID),
				MachineType: "n1-standard-1",
			},
			name:             fmt.Sprintf("profiler-test-gomaster-%s-gce", runID),
			wantProfileTypes: []string{"CPU", "HEAP", "THREADS", "CONTENTION", "HEAP_ALLOC"},
			goVersion:        "master",
			mutexProfiling:   true,
		},
		{
			InstanceConfig: proftest.InstanceConfig{
				ProjectID:   projectID,
				Zone:        zone,
				Name:        fmt.Sprintf("profiler-test-go111-%s", runID),
				MachineType: "n1-standard-1",
			},
			name:             fmt.Sprintf("profiler-test-go111-%s-gce", runID),
			wantProfileTypes: []string{"CPU", "HEAP", "THREADS", "CONTENTION", "HEAP_ALLOC"},
			goVersion:        "1.11",
			mutexProfiling:   true,
		},
		{
			InstanceConfig: proftest.InstanceConfig{
				ProjectID:   projectID,
				Zone:        zone,
				Name:        fmt.Sprintf("profiler-test-go110-%s", runID),
				MachineType: "n1-standard-1",
			},
			name:             fmt.Sprintf("profiler-test-go110-%s-gce", runID),
			wantProfileTypes: []string{"CPU", "HEAP", "THREADS", "CONTENTION", "HEAP_ALLOC"},
			goVersion:        "1.10",
			mutexProfiling:   true,
		},
		{
			InstanceConfig: proftest.InstanceConfig{
				ProjectID:   projectID,
				Zone:        zone,
				Name:        fmt.Sprintf("profiler-test-go19-%s", runID),
				MachineType: "n1-standard-1",
			},
			name:             fmt.Sprintf("profiler-test-go19-%s-gce", runID),
			wantProfileTypes: []string{"CPU", "HEAP", "THREADS", "CONTENTION", "HEAP_ALLOC"},
			goVersion:        "1.9",
			mutexProfiling:   true,
		},
	}
	// The number of tests run in parallel is the current value of GOMAXPROCS.
	runtime.GOMAXPROCS(len(testcases))
	for _, tc := range testcases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.initializeStartupScript(template, commit); err != nil {
				t.Fatalf("failed to initialize startup script")
			}

			if err := gceTr.StartInstance(ctx, &tc.InstanceConfig); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if gceTr.DeleteInstance(ctx, &tc.InstanceConfig); err != nil {
					t.Fatal(err)
				}
			}()

			timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute*25)
			defer cancel()
			if err := gceTr.PollForSerialOutput(timeoutCtx, &tc.InstanceConfig, benchFinishString, errorString); err != nil {
				t.Fatalf("PollForSerialOutput() got error: %v", err)
			}

			timeNow := time.Now()
			endTime := timeNow.Format(time.RFC3339)
			startTime := timeNow.Add(-1 * time.Hour).Format(time.RFC3339)
			for _, pType := range tc.wantProfileTypes {
				pr, err := tr.QueryProfiles(tc.ProjectID, tc.name, startTime, endTime, pType)
				if err != nil {
					t.Errorf("QueryProfiles(%s, %s, %s, %s, %s) got error: %v", tc.ProjectID, tc.name, startTime, endTime, pType, err)
					continue
				}
				if err := pr.HasFunction("busywork"); err != nil {
					t.Error(err)
				}
			}
		})
	}
}