/*
Copyright 2026.

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

// Command checkmanifestparity guards against the agent DaemonSet's
// kustomize manifest (config/agent/agent.yaml) and its Helm chart
// equivalent (hack/helm-postprocess.sh's heredoc, dist/chart/templates/
// extras/agent.yaml) drifting apart. Coordinator review (C6->C7): the
// chart's agent DaemonSet is a hand-maintained duplicate of the kustomize
// one, not derived from it, so a security-relevant edit to one (D14's
// securityContext, D28's narrowed cgroup hostPath) can be made without the
// other ever being touched. This compares the two rendered DaemonSets'
// security-relevant fields (pod/container securityContext, hostPID/
// hostNetwork, the cgroup/proc hostPath volumes and their mounts) and
// fails loudly on any difference.
//
// It does NOT compare unrelated fields (image, resources, labels,
// tolerations, ...): those are expected to differ or be independently
// configurable per §8/values.yaml.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "checkmanifestparity:", err)
		os.Exit(1)
	}
	fmt.Println("checkmanifestparity: kustomize and helm agent DaemonSets agree on every security-relevant field.")
}

func run() error {
	kustomizeBin := envOr("KUSTOMIZE_BIN", "kustomize")
	helmBin := envOr("HELM_BIN", "helm")
	chartDir := envOr("CHART_DIR", "dist/chart")

	kustomizeOut, err := runCmd(kustomizeBin, "build", "config/default")
	if err != nil {
		return fmt.Errorf("kustomize build config/default: %w", err)
	}
	helmOut, err := runCmd(helmBin, "template", "iolimiter-operator", chartDir, "--namespace", "iolimiter-operator-system")
	if err != nil {
		return fmt.Errorf("helm template %s: %w", chartDir, err)
	}

	kustomizeDS, err := findDaemonSet(kustomizeOut)
	if err != nil {
		return fmt.Errorf("finding agent DaemonSet in kustomize output: %w", err)
	}
	helmDS, err := findDaemonSet(helmOut)
	if err != nil {
		return fmt.Errorf("finding agent DaemonSet in helm output: %w", err)
	}

	return compareAgentDaemonSets(kustomizeDS, helmDS)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func runCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed binaries, caller-controlled args.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, stderr.String())
	}
	return out, nil
}

// findDaemonSet decodes every YAML document in rendered and returns the
// one DaemonSet whose name contains daemonSetNamePrefix + "-agent". Errors
// if none or more than one is found: either means the renderer's output
// shape changed in a way this check needs to know about.
func findDaemonSet(rendered []byte) (*appsv1.DaemonSet, error) {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(rendered)))
	var found []*appsv1.DaemonSet
	for {
		doc, err := reader.Read()
		if err != nil {
			break // io.EOF or a real error; either way, stop reading.
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		js, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("converting a rendered document to JSON: %w", err)
		}
		var u unstructured.Unstructured
		if err := u.UnmarshalJSON(js); err != nil {
			continue // not an object with a recognizable kind (e.g. an empty doc).
		}
		if u.GetKind() != "DaemonSet" {
			continue
		}
		var ds appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ds); err != nil {
			return nil, fmt.Errorf("decoding DaemonSet %s: %w", u.GetName(), err)
		}
		found = append(found, &ds)
	}
	var agentDS []*appsv1.DaemonSet
	for _, ds := range found {
		if strings.Contains(ds.Name, "agent") {
			agentDS = append(agentDS, ds)
		}
	}
	switch {
	case len(agentDS) == 0:
		return nil, fmt.Errorf("no DaemonSet found (saw %d DaemonSet(s) total)", len(found))
	case len(agentDS) > 1:
		return nil, fmt.Errorf("expected exactly one agent DaemonSet, found %d", len(agentDS))
	default:
		return agentDS[0], nil
	}
}

// compareAgentDaemonSets checks every D14/D28 security-relevant field.
// Collects every mismatch before returning, so a CI run reports all of
// them at once instead of one-at-a-time.
func compareAgentDaemonSets(kustomizeDS, helmDS *appsv1.DaemonSet) error {
	var mismatches []string
	check := func(field string, kustomizeVal, helmVal any) {
		if !reflect.DeepEqual(kustomizeVal, helmVal) {
			mismatches = append(mismatches, fmt.Sprintf("%s: kustomize=%#v helm=%#v", field, kustomizeVal, helmVal))
		}
	}

	kSpec, hSpec := kustomizeDS.Spec.Template.Spec, helmDS.Spec.Template.Spec
	check("spec.hostPID", kSpec.HostPID, hSpec.HostPID)
	check("spec.hostNetwork", kSpec.HostNetwork, hSpec.HostNetwork)
	check("spec.securityContext", kSpec.SecurityContext, hSpec.SecurityContext)

	if len(kSpec.Containers) != 1 || len(hSpec.Containers) != 1 {
		return fmt.Errorf("expected exactly one container in each agent DaemonSet, got kustomize=%d helm=%d",
			len(kSpec.Containers), len(hSpec.Containers))
	}
	kc, hc := kSpec.Containers[0], hSpec.Containers[0]
	check("container.securityContext", kc.SecurityContext, hc.SecurityContext)

	check("volumes[cgroup].hostPath",
		namedVolumeHostPath(kSpec.Volumes, "cgroup"), namedVolumeHostPath(hSpec.Volumes, "cgroup"))
	check("volumes[proc].hostPath",
		namedVolumeHostPath(kSpec.Volumes, "proc"), namedVolumeHostPath(hSpec.Volumes, "proc"))
	check("volumeMounts[cgroup]", namedVolumeMount(kc.VolumeMounts, "cgroup"), namedVolumeMount(hc.VolumeMounts, "cgroup"))
	check("volumeMounts[proc]", namedVolumeMount(kc.VolumeMounts, "proc"), namedVolumeMount(hc.VolumeMounts, "proc"))

	if len(mismatches) > 0 {
		var b strings.Builder
		b.WriteString("agent DaemonSet security-relevant fields differ between kustomize (config/agent/agent.yaml) and ")
		b.WriteString("helm (hack/helm-postprocess.sh) -- update both:\n")
		for _, m := range mismatches {
			b.WriteString("  - " + m + "\n")
		}
		return fmt.Errorf("%s", b.String())
	}
	return nil
}

func namedVolumeHostPath(vols []corev1.Volume, name string) *corev1.HostPathVolumeSource {
	for _, v := range vols {
		if v.Name == name {
			return v.HostPath
		}
	}
	return nil
}

func namedVolumeMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for _, m := range mounts {
		if m.Name == name {
			return &corev1.VolumeMount{Name: m.Name, MountPath: m.MountPath, ReadOnly: m.ReadOnly}
		}
	}
	return nil
}
