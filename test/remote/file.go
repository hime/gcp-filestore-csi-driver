/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remote

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/oauth2/google"
	filev1 "google.golang.org/api/file/v1"
	"k8s.io/klog"
)

const (
	retries = 10
	backoff = time.Second * 6
)

func GetFileClient() (*filev1.Service, error) {
	klog.V(4).Infof("Getting file client...")

	// Setup the file client for retrieving resources
	// Getting credentials on gce jenkins is flaky, so try a couple times
	var err error
	var fs *filev1.Service
	for i := 0; i < retries; i++ {
		if i > 0 {
			time.Sleep(backoff)
		}

		var client *http.Client
		client, err = google.DefaultClient(context.Background(), filev1.CloudPlatformScope)
		if err != nil {
			continue
		}

		fs, err = filev1.New(client)
		if err != nil {
			continue
		}
		return fs, nil
	}
	return nil, err
}
