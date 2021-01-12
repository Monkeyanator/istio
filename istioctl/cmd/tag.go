// Copyright Istio Authors
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

package cmd

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"text/tabwriter"

	"istio.io/istio/operator/pkg/helm"
	"istio.io/istio/pkg/kube"

	"github.com/hashicorp/go-multierror"
	"github.com/spf13/cobra"
	admit_v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"istio.io/api/label"
	"istio.io/istio/operator/pkg/helmreconciler"
)

const (
	// TODO(Monkeyanator) move into istio/api
	istioTagLabel             = "istio.io/tag"
	istioInjectionWebhookName = "sidecar-injector.istio.io"
	revisionTagNamePrefix     = "istio-revision-tag"

	// help strings and long formatted user outputs
	skipConfirmationFlagHelpStr = `The skipConfirmation determines whether the user is prompted for confirmation.
If set to true, the user is not prompted and a Yes response is assumed in all cases.`
	overrideHelpStr = `If true, allow revision tags to be overwritten, otherwise reject revision tag updates that
overwrite existing revision tags.`
	revisionHelpStr = "Control plane revision to reference from a given revision tag"
	tagCreatedStr   = `Revision tag %q created, referencing control plane revision %q. To enable injection using this
revision tag, use 'kubectl label namespace <NAMESPACE> istio.io/rev=%s'
`
)

var (
	// revision to point tag webhook at
	revision         = ""
	overwrite        = false
	skipConfirmation = false
)

type tagWebhookConfig struct {
	tag                string
	revision           string
	remoteInjectionURL string
}

func tagCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag",
		Short: "Command group used to interact with revision tags",
		Long: `Command group used to interact with revision tags. Revision tags allow for the creation of mutable aliases
referring to control plane revisions for sidecar injection.

With revision tags, rather than relabeling a namespace from "istio.io/rev=revision-a" to "istio.io/rev=revision-b" to
change which control plane revision handles injection, it's possible to create a revision tag "prod" and label our 
namespace "istio.io/rev=prod". The "prod" revision tag could point to "1-7-6" initially and then be changed to point to "1-8-1"
at some later point.

This allows operators to change which Istio control plane revision should handle injection for a namespace or set of namespaces
without manual relabeling of the "istio.io/rev" tag.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.HelpFunc()(cmd, args)
			if len(args) != 0 {
				return fmt.Errorf("unknown subcommand %q", args[0])
			}

			return nil
		},
	}

	cmd.AddCommand(tagSetCommand())
	cmd.AddCommand(tagListCommand())
	cmd.AddCommand(tagRemoveCommand())

	return cmd
}

func tagSetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <revision-tag>",
		Short: "Create or modify revision tags",
		Long: `Create or modify revision tags. Tag an Istio control plane revision for use with namespace istio.io/rev
injection labels.`,
		Example: ` # Create a revision tag from the "1-8-0" revision
	istioctl x tag set prod --revision 1-8-0

	# Point namespace "test-ns" at the revision pointed to by the "prod" revision tag
	kubectl label ns test-ns istio.io/rev=prod

	# Change the revision tag to reference the "1-8-1" revision
	istioctl x tag set prod --revision 1-8-1 --overwrite

	# Rollout namespace "test-ns" to update workloads to the "1-8-1" revision
	kubectl rollout restart deployments -n test-ns
`,
		SuggestFor: []string{"create"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("must provide a tag for modification")
			}
			if len(args) > 1 {
				return fmt.Errorf("must provide a single tag for creation")
			}

			client, err := kubeClient(kubeconfig, configContext)
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes client: %v", err)
			}

			return setTag(context.Background(), client, args[0], revision, cmd.OutOrStdout())
		},
	}

	cmd.PersistentFlags().BoolVar(&overwrite, "overwrite", false, overrideHelpStr)
	cmd.PersistentFlags().BoolVarP(&skipConfirmation, "skip-confirmation", "y", false, skipConfirmationFlagHelpStr)
	cmd.PersistentFlags().StringVarP(&revision, "revision", "r", "", revisionHelpStr)
	_ = cmd.MarkPersistentFlagRequired("revision")

	return cmd
}

func tagListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List existing revision tags",
		Example: "istioctl x tag list",
		Aliases: []string{"show"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("tag list command does not accept arguments")
			}

			client, err := kubeClient(kubeconfig, configContext)
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes client: %v", err)
			}
			return listTags(context.Background(), client.Kube(), cmd.OutOrStdout())
		},
	}

	return cmd
}

func tagRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <revision-tag>",
		Short: "Remove Istio control plane revision tag",
		Long: `Remove Istio control plane revision tag.

Removing a revision tag should be done with care. Removing a revision tag will disrupt sidecar injection in namespaces
that reference the tag in an "istio.io/rev" label. Verify that there are no remaining namespaces referencing a
revision tag before removing using the "istioctl x tag list" command.
`,
		Example: ` # Remove the revision tag "prod"
	istioctl x tag remove prod
`,
		Aliases: []string{"delete"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("must provide a tag for removal")
			}
			if len(args) > 1 {
				return fmt.Errorf("must provide a single tag for removal")
			}

			client, err := kubeClient(kubeconfig, configContext)
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes client: %v", err)
			}

			return removeTag(context.Background(), client.Kube(), args[0], skipConfirmation, cmd.OutOrStdout())
		},
	}

	return cmd
}

// setTag creates or modifies a revision tag.
func setTag(ctx context.Context, kubeClient kube.ExtendedClient, tag, revision string, w io.Writer) error {
	// abort if there exists a revision with the target tag name
	revWebhookCollisions, err := getWebhooksWithRevision(ctx, kubeClient, tag)
	if err != nil {
		return err
	}
	if len(revWebhookCollisions) > 0 {
		return fmt.Errorf("cannot create revision tag %q: found existing control plane revision with same name", tag)
	}

	// find canonical revision webhook to base our tag webhook off of
	revWebhooks, err := getWebhooksWithRevision(ctx, kubeClient, revision)
	if err != nil {
		return err
	}
	if len(revWebhooks) == 0 {
		return fmt.Errorf("cannot modify tag: cannot find MutatingWebhookConfiguration with revision %q", revision)
	}
	if len(revWebhooks) > 1 {
		return fmt.Errorf("cannot modify tag: found multiple canonical webhooks with revision %q", revision)
	}

	whs, err := getWebhooksWithTag(ctx, kubeClient, tag)
	if err != nil {
		return err
	}
	if len(whs) > 0 && !overwrite {
		return fmt.Errorf("revision tag %q already exists, and --overwrite is false", tag)
	}

	tagWhConfig, err := tagWebhookConfigFromCanonicalWebhook(revWebhooks[0], tag)
	if err != nil {
		return fmt.Errorf("failed to create tag webhook config: %v", err)
	}
	if err := createTagWebhook(kubeClient, tagWhConfig); err != nil {
		return fmt.Errorf("failed to create tag webhook: %v", err)
	}

	fmt.Fprintf(w, tagCreatedStr, tag, revision, tag)
	return nil
}

// removeTag removes an existing revision tag.
func removeTag(ctx context.Context, kubeClient kubernetes.Interface, tag string, skipConfirmation bool, w io.Writer) error {
	webhooks, err := getWebhooksWithTag(ctx, kubeClient, tag)
	if err != nil {
		return fmt.Errorf("failed to retrieve tag with name %s: %v", tag, err)
	}
	if len(webhooks) == 0 {
		return fmt.Errorf("cannot remove tag %q: cannot find MutatingWebhookConfiguration for tag", tag)
	}

	taggedNamespaces, err := getNamespacesWithTag(ctx, kubeClient, tag)
	if err != nil {
		return fmt.Errorf("failed to retrieve namespaces dependent on tag %q", tag)
	}
	// warn user if deleting a tag that still has namespaces pointed to it
	if len(taggedNamespaces) > 0 && !skipConfirmation {
		if !confirm(buildDeleteTagConfirmation(tag, taggedNamespaces), w) {
			fmt.Fprintf(w, "Aborting operation.\n")
			return nil
		}
	}

	// proceed with webhook deletion
	err = deleteTagWebhooks(ctx, kubeClient, webhooks)
	if err != nil {
		return fmt.Errorf("failed to delete Istio revision tag MutatingConfigurationWebhook: %v", err)
	}

	fmt.Fprintf(w, "Revision tag %s removed\n", tag)
	return nil
}

// listTags lists existing revision.
func listTags(ctx context.Context, kubeClient kubernetes.Interface, writer io.Writer) error {
	tagWebhooks, err := getTagWebhooks(ctx, kubeClient)
	if err != nil {
		return fmt.Errorf("failed to retrieve revision tags: %v", err)
	}
	if len(tagWebhooks) == 0 {
		fmt.Fprintf(writer, "No Istio revision tag MutatingWebhookConfigurations to list")
		return nil
	}
	w := new(tabwriter.Writer).Init(writer, 0, 8, 1, ' ', 0)
	fmt.Fprintln(w, "TAG\tREVISION\tNAMESPACES")
	for _, wh := range tagWebhooks {
		tagName, err := getWebhookName(wh)
		if err != nil {
			return fmt.Errorf("error parsing tag name from webhook %q: %v", wh.Name, err)
		}
		tagRevision, err := getWebhookRevision(wh)
		if err != nil {
			return fmt.Errorf("error parsing revision from webhook %q: %v", wh.Name, err)
		}
		tagNamespaces, err := getNamespacesWithTag(ctx, kubeClient, tagName)
		if err != nil {
			return fmt.Errorf("error retrieving namespaces for tag %q: %v", tagName, err)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\n", tagName, tagRevision, strings.Join(tagNamespaces, ","))
	}

	return w.Flush()
}

// getTagWebhooks returns all webhooks tagged with istio.io/tag.
func getTagWebhooks(ctx context.Context, client kubernetes.Interface) ([]admit_v1.MutatingWebhookConfiguration, error) {
	webhooks, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{
		LabelSelector: istioTagLabel,
	})
	if err != nil {
		return nil, err
	}
	return webhooks.Items, nil
}

// getWebhooksWithTag returns webhooks tagged with istio.io/tag=<tag>.
func getWebhooksWithTag(ctx context.Context, client kubernetes.Interface, tag string) ([]admit_v1.MutatingWebhookConfiguration, error) {
	webhooks, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", istioTagLabel, tag),
	})
	if err != nil {
		return nil, err
	}
	return webhooks.Items, nil
}

// getWebhooksWithRevision returns webhooks tagged with istio.io/rev=<rev> and NOT TAGGED with istio.io/tag.
// this retrieves the webhook created at revision installation rather than tag webhooks
func getWebhooksWithRevision(ctx context.Context, client kubernetes.Interface, rev string) ([]admit_v1.MutatingWebhookConfiguration, error) {
	webhooks, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,!%s", label.IstioRev, rev, istioTagLabel),
	})
	if err != nil {
		return nil, err
	}
	return webhooks.Items, nil
}

// getNamespacesWithTag retrieves all namespaces pointed at the given tag.
func getNamespacesWithTag(ctx context.Context, client kubernetes.Interface, tag string) ([]string, error) {
	namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", label.IstioRev, tag),
	})
	if err != nil {
		return nil, err
	}

	nsNames := make([]string, len(namespaces.Items))
	for i, ns := range namespaces.Items {
		nsNames[i] = ns.Name
	}
	return nsNames, nil
}

// getWebhookName extracts tag name from webhook object.
func getWebhookName(wh admit_v1.MutatingWebhookConfiguration) (string, error) {
	if tagName, ok := wh.ObjectMeta.Labels[istioTagLabel]; ok {
		return tagName, nil
	}
	return "", fmt.Errorf("could not extract tag name from webhook")
}

// getRevision extracts tag target revision from webhook object.
func getWebhookRevision(wh admit_v1.MutatingWebhookConfiguration) (string, error) {
	if tagName, ok := wh.ObjectMeta.Labels[label.IstioRev]; ok {
		return tagName, nil
	}
	return "", fmt.Errorf("could not extract tag revision from webhook")
}

// deleteTagWebhooks deletes the given webhooks.
func deleteTagWebhooks(ctx context.Context, client kubernetes.Interface, webhooks []admit_v1.MutatingWebhookConfiguration) error {
	var result error
	for _, wh := range webhooks {
		result = multierror.Append(client.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, wh.Name, metav1.DeleteOptions{})).ErrorOrNil()
	}
	return result
}

// buildDeleteTagConfirmation takes a list of webhooks and creates a message prompting confirmation for their deletion.
func buildDeleteTagConfirmation(tag string, taggedNamespaces []string) string {
	var sb strings.Builder
	base := fmt.Sprintf("Caution, found %d namespace(s) still injected by tag %q:", len(taggedNamespaces), tag)
	sb.WriteString(base)
	for _, ns := range taggedNamespaces {
		sb.WriteString(" " + ns)
	}
	sb.WriteString("\nProceed with operation? [y/N]")

	return sb.String()
}

// buildTagWebhookFromCanonical takes a canonical injector webhook for a given revision and generates a tag webhook.
// from the original webhook, we need to change (1) the namespace selector (2) the name (3) the labels
func buildTagWebhookFromCanonical(wh admit_v1.MutatingWebhookConfiguration, tag, revision string) (*admit_v1.MutatingWebhookConfiguration, error) {
	tagWebhook := &admit_v1.MutatingWebhookConfiguration{}
	tagWebhook.Name = fmt.Sprintf("%s-%s", revisionTagNamePrefix, tag)
	tagWebhookLabels := map[string]string{
		istioTagLabel:  tag,
		label.IstioRev: revision,
		// needed so istioctl uninstall can cleanup tag webhooks
		helmreconciler.IstioComponentLabelStr: "Pilot",
	}
	tagWebhook.Labels = tagWebhookLabels
	injectionWebhook, err := buildInjectionWebhook(wh, tag)
	if err != nil {
		return nil, err
	}
	tagWebhook.Webhooks = []admit_v1.MutatingWebhook{
		injectionWebhook,
	}
	return tagWebhook, nil
}

// buildInjectionWebhook takes a webhook configuration, copies the injection webhook, and changes key fields.
func buildInjectionWebhook(wh admit_v1.MutatingWebhookConfiguration, tag string) (admit_v1.MutatingWebhook, error) {
	var injectionWebhook *admit_v1.MutatingWebhook
	for _, w := range wh.Webhooks {
		if w.Name == istioInjectionWebhookName {
			injectionWebhook = w.DeepCopy()
		}
	}
	if injectionWebhook == nil {
		return admit_v1.MutatingWebhook{}, fmt.Errorf("injection webhook not found")
	}

	// webhook should inject for istio.io/rev=<tag>
	tagWebhookNamespaceSelector := metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key: label.IstioRev, Operator: metav1.LabelSelectorOpIn, Values: []string{tag},
			},
			{
				Key: "istio-injection", Operator: metav1.LabelSelectorOpDoesNotExist,
			},
		},
	}
	injectionWebhook.NamespaceSelector = &tagWebhookNamespaceSelector
	injectionWebhook.ClientConfig.CABundle = []byte("") // istiod patches the CA bundle in
	return *injectionWebhook, nil
}

func tagWebhookConfigFromCanonicalWebhook(wh admit_v1.MutatingWebhookConfiguration, tag string) (*tagWebhookConfig, error) {
	rev, err := getWebhookRevision(wh)
	if err != nil {
		return nil, err
	}
	var injectionURL string
	found := false
	for _, w := range wh.Webhooks {
		if w.Name == istioInjectionWebhookName {
			found = true
			if w.ClientConfig.URL != nil {
				injectionURL = *w.ClientConfig.URL
			} else {
				injectionURL = ""
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("could not find sidecar-injector webhook in canonical webhook")
	}

	return &tagWebhookConfig{
		tag:                tag,
		revision:           rev,
		remoteInjectionURL: injectionURL,
	}, nil
}

func createTagWebhook(kubeClient kube.ExtendedClient, config *tagWebhookConfig) error {
	r, err := helm.NewHelmRenderer("", "istio-control/istio-discovery", "Pilot", "istio-system")
	if err != nil {
		return fmt.Errorf("failed creating Helm renderer: %v", err)
	}

	if err := r.Run(); err != nil {
		return fmt.Errorf("failed running Helm renderer: %v", err)
	}

	values := fmt.Sprintf(`
revision: %s
revisionTags: [%s]

global:
  istioNamespace: istio-system
  istiod:
    enableAnalysis: false
  configValidation: false

istiodRemote:
  injectionURL: %s

base:
  enableCRDTemplates: false
`, config.revision, config.tag, config.remoteInjectionURL)
	fmt.Println(values)

	tagWebhookYaml, err := r.RenderManifest(values)
	if err != nil {
		return fmt.Errorf("failed rendering istio-control manifest: %v", err)
	}

	return applyYAML(kubeClient, tagWebhookYaml, "istio-system")
}

func applyYAML(client kube.ExtendedClient, yamlContent, ns string) error {
	yamlFile, err := writeToTempFile(yamlContent)
	if err != nil {
		return fmt.Errorf("failed creating manifest file: %v", err)
	}

	// Apply the YAML to the cluster.
	if err := client.ApplyYAMLFiles(ns, yamlFile); err != nil {
		return fmt.Errorf("failed applying manifest %s: %v", yamlFile, err)
	}
	return nil
}

func writeToTempFile(content string) (string, error) {
	outFile, err := ioutil.TempFile("", "remote-secret-manifest-*")
	if err != nil {
		return "", fmt.Errorf("failed creating temp file for manifest: %v", err)
	}
	defer func() { _ = outFile.Close() }()

	if _, err := outFile.Write([]byte(content)); err != nil {
		return "", fmt.Errorf("failed writing manifest file: %v", err)
	}
	return outFile.Name(), nil
}

// TODO(monkeyanator) duplicated between cmd/mesh/upgrade.go, move to common place
// confirm waits for a user to confirm with the supplied message.
func confirm(msg string, w io.Writer) bool {
	fmt.Fprintf(w, "%s ", msg)

	var response string
	_, err := fmt.Scanln(&response)
	if err != nil {
		return false
	}
	response = strings.ToUpper(response)
	return response == "Y" || response == "YES"
}
