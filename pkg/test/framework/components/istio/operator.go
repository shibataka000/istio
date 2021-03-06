// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package istio

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"istio.io/istio/pkg/test/deployment"
	"istio.io/istio/pkg/test/framework/components/environment/kube"
	"istio.io/istio/pkg/test/framework/components/istioctl"
	"istio.io/istio/pkg/test/framework/image"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/scopes"
)

func DefaultValidatingWebhookConfigurationName(config Config) string {
	return fmt.Sprintf("istiod-%v", config.SystemNamespace)

}

const (
	DefaultMutatingWebhookConfigurationName = "istio-sidecar-injector"
)

type operatorComponent struct {
	id          resource.ID
	settings    Config
	ctx         resource.Context
	environment *kube.Environment
}

var _ io.Closer = &operatorComponent{}
var _ Instance = &operatorComponent{}
var _ resource.Dumper = &operatorComponent{}

// ID implements resource.Instance
func (i *operatorComponent) ID() resource.ID {
	return i.id
}

func (i *operatorComponent) Settings() Config {
	return i.settings
}

func (i *operatorComponent) Close() (err error) {
	scopes.CI.Infof("=== BEGIN: Cleanup Istio ===")
	defer scopes.CI.Infof("=== DONE: Cleanup Istio ===")
	if i.settings.DeployIstio {
		err = i.environment.KubeClusters[0].DeleteNamespace(i.settings.SystemNamespace)
		if err == nil {
			err = i.environment.KubeClusters[0].WaitForNamespaceDeletion(i.settings.SystemNamespace)
		}
		// Note: when cleaning up an Istio deployment, ValidatingWebhookConfiguration
		// and MutatingWebhookConfiguration must be cleaned up. Otherwise, next
		// Istio deployment in the cluster will be impacted, causing flaky test results.
		// Clean up ValidatingWebhookConfiguration and MutatingWebhookConfiguration if they exist
		_ = i.environment.KubeClusters[0].DeleteValidatingWebhook(DefaultValidatingWebhookConfigurationName(i.settings))
		_ = i.environment.KubeClusters[0].DeleteMutatingWebhook(DefaultMutatingWebhookConfigurationName)
	}
	return
}

func (i *operatorComponent) Dump() {
	scopes.CI.Errorf("=== Dumping Istio Deployment State...")

	d, err := i.ctx.CreateTmpDirectory("istio-state")
	if err != nil {
		scopes.CI.Errorf("Unable to create directory for dumping Istio contents: %v", err)
		return
	}

	deployment.DumpPodState(d, i.settings.SystemNamespace, i.environment.KubeClusters[0].Accessor)
	deployment.DumpPodEvents(d, i.settings.SystemNamespace, i.environment.KubeClusters[0].Accessor)

	pods, err := i.environment.KubeClusters[0].GetPods(i.settings.SystemNamespace)
	if err != nil {
		scopes.CI.Errorf("Unable to get pods from the system namespace: %v", err)
		return
	}

	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			l, err := i.environment.KubeClusters[0].Logs(pod.Namespace, pod.Name, container.Name, false /* previousLog */)
			if err != nil {
				scopes.CI.Errorf("Unable to get logs for pod/container: %s/%s/%s", pod.Namespace, pod.Name, container.Name)
				continue
			}

			fname := path.Join(d, fmt.Sprintf("%s-%s.log", pod.Name, container.Name))
			if err = ioutil.WriteFile(fname, []byte(l), os.ModePerm); err != nil {
				scopes.CI.Errorf("Unable to write logs for pod/container: %s/%s/%s", pod.Namespace, pod.Name, container.Name)
			}
		}
	}
}

func deploy(ctx resource.Context, env *kube.Environment, cfg Config) (Instance, error) {
	scopes.CI.Infof("=== Istio Component Config ===")
	scopes.CI.Infof("\n%s", cfg.String())
	scopes.CI.Infof("================================")

	i := &operatorComponent{
		environment: env,
		settings:    cfg,
		ctx:         ctx,
	}
	i.id = ctx.TrackResource(i)

	if !cfg.DeployIstio {
		scopes.Framework.Info("skipping deployment due to Config")
		return i, nil
	}

	istioCtl, err := istioctl.New(ctx, istioctl.Config{})
	if err != nil {
		return nil, err
	}

	// Top-level work dir for Istio deployment.
	workDir, err := ctx.CreateTmpDirectory("istio-deployment")
	if err != nil {
		return nil, err
	}

	iopFile := filepath.Join(workDir, "iop.yaml")
	if err := ioutil.WriteFile(iopFile, []byte(cfg.IstioOperator()), os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to write iop: %v", err)
	}
	s, err := image.SettingsFromCommandLine()
	if err != nil {
		return nil, err
	}
	cmd := []string{
		"manifest", "apply",
		"--skip-confirmation",
		"--logtostderr",
		"-f", iopFile,
		"--set", "values.global.imagePullPolicy=" + s.PullPolicy,
		"--wait",
	}
	// If control plane values set, assume this includes the full set of values, and .Values is
	// just for helm use case. Otherwise, include all values.
	if cfg.ControlPlaneValues == "" {
		for k, v := range cfg.Values {
			cmd = append(cmd, "--set", fmt.Sprintf("values.%s=%s", k, v))
		}
	}

	scopes.CI.Infof("Running istioctl %v", cmd)
	if _, err := istioCtl.Invoke(cmd); err != nil {
		return nil, fmt.Errorf("manifest apply failed: %v", err)
	}

	if !cfg.SkipWaitForValidationWebhook {
		// Wait for the validation webhook to come online before continuing.
		if _, _, err = env.KubeClusters[0].WaitUntilServiceEndpointsAreReady(cfg.SystemNamespace, "istiod"); err != nil {
			err = fmt.Errorf("error waiting %s/%s service endpoints: %v", cfg.SystemNamespace, "istiod", err)
			scopes.CI.Info(err.Error())
			i.Dump()
			return nil, err
		}

		// Wait for webhook to come online. The only reliable way to do that is to see if we can submit invalid config.
		err = waitForValidationWebhook(env.KubeClusters[0].Accessor, cfg)
		if err != nil {
			i.Dump()
			return nil, err
		}
	}

	return i, nil
}
