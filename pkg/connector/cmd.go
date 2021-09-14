// Copyright 2021 FabEdge Team
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

package connector

import (
	flag "github.com/spf13/pflag"
	"k8s.io/klog/v2"

	"github.com/fabedge/fabedge/pkg/common/about"
	logutil "github.com/fabedge/fabedge/pkg/util/log"
)

func Execute() {
	defer klog.Flush()

	fs := flag.CommandLine
	cfg := &Config{}

	logutil.AddFlags(fs)
	about.AddFlags(fs)
	cfg.AddFlags(fs)

	flag.Parse()

	about.DisplayAndExitIfRequested()

	manger, err := cfg.Manager()
	if err != nil {
		klog.Fatalf("failed to create Manager: %s", err)
	}

	manger.Start()
}
