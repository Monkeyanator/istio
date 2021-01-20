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
	"strings"
	"text/tabwriter"

	"github.com/hashicorp/go-multierror"
	"github.com/spf13/cobra"
	admit_v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"istio.io/api/label"
)

const (
	// TODO(monkeyanator) move into istio/api
	istioTagLabel             = "istio.io/tag"
	istioInjectionWebhookName = "sidecar-injector.istio.io"
	revisionTagNamePrefix     = "istio-revision-tag"
)

var (
	// revision to point tag webhook at
	revision         = ""
	overwrite        = false
	skipConfirmation = false
)

func tagCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag",
		Short: "Command group used to interact with revision tags",
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
		Use:        "set",
		Short:      "Create or modify revision tags",
		Example:    "istioctl x tag set prod --revision 1-8-0",
		SuggestFor: []string{"create"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("must provide a tag for modification")
			}
			if len(args) > 1 {
				return fmt.Errorf("can only provide a single tag for creation")
			}

			client, err := kubeClient(kubeconfig, configContext)
			if err != nil {
				return fmt.Errorf("failed to create kubernetes client: %v", err)
			}

			return setTag(context.Background(), client.Kube(), args[0], revision, cmd.OutOrStdout())
		},
	}

	cmd.PersistentFlags().BoolVar(&overwrite, "overwrite", false, "whether to overwrite an existing tag")
	cmd.PersistentFlags().BoolVarP(&skipConfirmation, "skip-confirmation", "y", false, "whether to skip confirmation for tag deletion")
	cmd.PersistentFlags().StringVarP(&revision, "revision", "r", "", "revision to point tag to")
	_ = cmd.MarkPersistentFlagRequired("revision")

	return cmd
}

func tagListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List existing revision tags and their corresponding revisions",
		Example: "istioctl x tag list",
		Aliases: []string{"show"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("tag list command does not accept arguments")
			}

			client, err := kubeClient(kubeconfig, configContext)
			if err != nil {
				return fmt.Errorf("failed to create kubernetes client: %v", err)
			}
			return listTags(context.Background(), client.Kube(), cmd.OutOrStdout())
		},
	}

	return cmd
}

func tagRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove",
		Short:   "Remove an existing revision tag",
		Example: "istioctl x tag remove prod",
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
				return fmt.Errorf("failed to create kubernetes client: %v", err)
			}

			return removeTag(context.Background(), client.Kube(), args[0], skipConfirmation, cmd.OutOrStdout())
		},
	}

	return cmd
}

// setTag creates or modifies a revision tag.
func setTag(ctx context.Context, kubeClient kubernetes.Interface, tag, revision string, w io.Writer) error {
	revWebhooks, err := getWebhooksWithRevision(ctx, kubeClient, revision)
	if err != nil {
		return err
	}
	if len(revWebhooks) == 0 {
		return fmt.Errorf("cannot find webhook with revision %q", revision)
	}
	if len(revWebhooks) > 1 {
		return fmt.Errorf("found multiple canonical webhooks for revision %q", revision)
	}

	tagWebhook, err := buildTagWebhookFromCanonical(revWebhooks[0], tag, revision)
	if err != nil {
		return err
	}

	whs, err := getWebhooksWithTag(ctx, kubeClient, tag)
	if len(whs) > 0 && !overwrite {
		return fmt.Errorf("found an existing revision tag for %q, use the --overwrite flag to overwrite", tag)
	}

	// create or update webhook depending on whether it exists
	// note there should be a single webhook for each revision tag
	kubeWebhookClient := kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations()
	if len(whs) == 0 {
		_, err = kubeWebhookClient.Create(ctx, tagWebhook, metav1.CreateOptions{})
	} else {
		tagWebhook.ObjectMeta.ResourceVersion = whs[0].ObjectMeta.ResourceVersion
		_, err = kubeWebhookClient.Update(ctx, tagWebhook, metav1.UpdateOptions{})
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "Revision tag %s created\n", tag)
	return nil
}

// removeTag removes an existing revision tag.
func removeTag(ctx context.Context, kubeClient kubernetes.Interface, tag string, skipConfirmation bool, w io.Writer) error {
	webhooks, err := getWebhooksWithTag(ctx, kubeClient, tag)
	if err != nil {
		return fmt.Errorf("failed to retrieve tag with name %s: %v", tag, err)
	}
	if len(webhooks) == 0 {
		return fmt.Errorf("revision tag %s does not exist", tag)
	}

	taggedNamespaces, err := getNamespacesWithTag(ctx, kubeClient, tag)
	if err != nil {
		return fmt.Errorf("could not retrieve namespaces dependent on tag: %s", tag)
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
		return err
	}

	fmt.Fprintf(w, "Revision tag %s removed\n", tag)
	return nil
}

// listTags lists existing revision.
func listTags(ctx context.Context, kubeClient kubernetes.Interface, writer io.Writer) error {
	tagWebhooks, err := getTagWebhooks(ctx, kubeClient)
	if err != nil {
		return fmt.Errorf("failed to retrieve tags: %v", err)
	}
	if len(tagWebhooks) == 0 {
		fmt.Fprintf(writer, "No tag webhooks found\n")
		return nil
	}
	w := new(tabwriter.Writer).Init(writer, 0, 8, 1, ' ', 0)
	fmt.Fprintln(w, "TAG\tREVISION\tNAMESPACES")
	for _, wh := range tagWebhooks {
		tagName, err := getTagName(wh)
		if err != nil {
			return fmt.Errorf("error parsing webhook %q: %v", wh.Name, err)
		}
		tagRevision, err := getTagRevision(wh)
		if err != nil {
			return fmt.Errorf("error parsing webhook %q: %v", wh.Name, err)
		}
		tagNamespaces, err := getNamespacesWithTag(ctx, kubeClient, tagName)
		if err != nil {
			return fmt.Errorf("error parsing webhook %q: %v", wh.Name, err)
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

// getTagName extracts tag name from webhook object.
func getTagName(wh admit_v1.MutatingWebhookConfiguration) (string, error) {
	if tagName, ok := wh.ObjectMeta.Labels[istioTagLabel]; ok {
		return tagName, nil
	}
	return "", fmt.Errorf("could not extract tag name from webhook")
}

// getRevision extracts tag target revision from webhook object.
func getTagRevision(wh admit_v1.MutatingWebhookConfiguration) (string, error) {
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
	base := fmt.Sprintf("Caution, found %d namespace(s) still pointing to tag %q:", len(taggedNamespaces), tag)
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
	tagWebhookLabels := map[string]string{istioTagLabel: tag, label.IstioRev: revision}
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
