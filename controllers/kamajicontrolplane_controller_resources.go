// Copyright 2023 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/clastix/cluster-api-control-plane-provider-kamaji/api/v1alpha1"
)

var ErrEnqueueBack = errors.New("enqueue back")

//+kubebuilder:rbac:groups="",resources="secrets",verbs=get;list;watch;create;update;patch

func (r *KamajiControlPlaneReconciler) createRequiredResources(ctx context.Context, remoteClient client.Client, cluster capiv1beta1.Cluster, kcp v1alpha1.KamajiControlPlane, tcp *kamajiv1alpha1.TenantControlPlane) error {
	log := ctrllog.FromContext(ctx)
	// Creating a kubeconfig secret for the workload cluster.
	if secretName := tcp.Status.KubeConfig.Admin.SecretName; len(secretName) == 0 {
		log.Info("admin kubeconfig still unprocessed by Kamaji, unable to create kubeconfig secret for the workload cluster, enqueuing back")

		return fmt.Errorf("admin kubeconfig still unprocessed by Kamaji, %w", ErrEnqueueBack)
	}

	reader := r.client

	if remoteClient != nil {
		reader = remoteClient
	}

	if err := r.createOrUpdateKubeconfig(ctx, reader, cluster, kcp, tcp); err != nil {
		log.Error(err, "unable to replicate kubeconfig secret for the workload cluster")

		return err
	}
	// Creating a CA secret for the workload cluster.
	if secretName := tcp.Status.Certificates.CA.SecretName; len(secretName) == 0 {
		log.Info("CA still unprocessed by Kamaji, unable to create Certificate Authority secret for the workload cluster, enqueuing back")

		return fmt.Errorf("CA still unprocessed by Kamaji, %w", ErrEnqueueBack)
	}

	if err := r.createOrUpdateCertificateAuthority(ctx, reader, cluster, kcp, tcp); err != nil {
		log.Error(err, "unable to replicate CA secret for the workload cluster")

		return err
	}

	return nil
}

// createOrUpdateCertificateAuthority takes care of translating corev1.Secret from Kamaji to CAPI expected resource,
// also in regard to the naming conventions according to the Cluster API contracts about Kubeconfig.
//
// more info: https://cluster-api.sigs.k8s.io/developer/architecture/controllers/cluster.html#secrets
func (r *KamajiControlPlaneReconciler) createOrUpdateCertificateAuthority(ctx context.Context, reader client.Client, cluster capiv1beta1.Cluster, kcp v1alpha1.KamajiControlPlane, tcp *kamajiv1alpha1.TenantControlPlane) error {
	capiCA := &corev1.Secret{}
	capiCA.Name = cluster.Name + "-ca"
	capiCA.Namespace = cluster.Namespace

	kamajiCA := &corev1.Secret{}
	kamajiCA.Name = tcp.Status.Certificates.CA.SecretName
	kamajiCA.Namespace = tcp.Namespace

	if err := reader.Get(ctx, types.NamespacedName{Name: kamajiCA.Name, Namespace: kamajiCA.Namespace}, kamajiCA); err != nil {
		return errors.Wrap(err, "cannot retrieve source-of-truth as Certificate Authority")
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, scopeErr := controllerutil.CreateOrUpdate(ctx, r.client, capiCA, func() error {
			// Skipping the replication of the Certificate Authority if the Secret is managed by the Kamaji operator
			if len(capiCA.GetOwnerReferences()) > 0 && capiCA.OwnerReferences[0].Kind == "TenantControlPlane" {
				return nil
			}

			crt, found := kamajiCA.Data["ca.crt"]
			if !found {
				return errors.New("missing Certificate value from *kamajiv1alpha1.TenantControlPlane CA")
			}

			key, found := kamajiCA.Data["ca.key"]
			if !found {
				return errors.New("missing Private Key value from *kamajiv1alpha1.TenantControlPlane CA")
			}

			labels := kamajiCA.Labels
			if labels == nil {
				labels = map[string]string{}
			}

			labels[capiv1beta1.ClusterNameLabel] = cluster.Name
			labels["kamaji.clastix.io/component"] = "capi"
			labels["kamaji.clastix.io/secret"] = "ca"
			labels["kamaji.clastix.io/cluster"] = cluster.Name
			labels["kamaji.clastix.io/tcp"] = tcp.Name

			capiCA.SetLabels(labels)

			capiCA.Data = map[string][]byte{
				corev1.TLSCertKey:       crt,
				corev1.TLSPrivateKeyKey: key,
			}
			capiCA.Type = capiv1beta1.ClusterSecretType

			return controllerutil.SetControllerReference(&kcp, capiCA, r.client.Scheme())
		})

		return scopeErr //nolint:wrapcheck
	})
	if err != nil {
		return errors.Wrap(err, "cannot create or update CA secret")
	}

	return nil
}

// createOrUpdateKubeconfig takes care of translating corev1.Secret from Kamaji to CAPI expected resource,
// also in regard to the naming conventions according to the Cluster API contracts about kubeconfig.
//
// more info: https://cluster-api.sigs.k8s.io/developer/architecture/controllers/cluster.html#secrets
func (r *KamajiControlPlaneReconciler) createOrUpdateKubeconfig(ctx context.Context, reader client.Client, cluster capiv1beta1.Cluster, kcp v1alpha1.KamajiControlPlane, tcp *kamajiv1alpha1.TenantControlPlane) error {
	capiAdminKubeconfig := &corev1.Secret{}
	capiAdminKubeconfig.Name = cluster.Name + "-kubeconfig"
	capiAdminKubeconfig.Namespace = cluster.Namespace

	kamajiAdminKubeconfig := &corev1.Secret{}
	kamajiAdminKubeconfig.Name = tcp.Status.KubeConfig.Admin.SecretName
	kamajiAdminKubeconfig.Namespace = tcp.Namespace

	if err := reader.Get(ctx, types.NamespacedName{Name: kamajiAdminKubeconfig.Name, Namespace: kamajiAdminKubeconfig.Namespace}, kamajiAdminKubeconfig); err != nil {
		return errors.Wrap(err, "cannot retrieve source-of-truth for admin kubeconfig")
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, scopeErr := controllerutil.CreateOrUpdate(ctx, r.client, capiAdminKubeconfig, func() error {
			labels := capiAdminKubeconfig.Labels
			if labels == nil {
				labels = map[string]string{}
			}

			labels[capiv1beta1.ClusterNameLabel] = cluster.Name
			labels["kamaji.clastix.io/component"] = "capi"
			labels["kamaji.clastix.io/secret"] = "kubeconfig"
			labels["kamaji.clastix.io/cluster"] = cluster.Name
			labels["kamaji.clastix.io/tcp"] = tcp.Name

			secretKey := "admin.conf"
			if v, ok := kcp.GetAnnotations()[kamajiv1alpha1.KubeconfigSecretKeyAnnotation]; ok && v != "" {
				secretKey = v
			}

			value, ok := kamajiAdminKubeconfig.Data[secretKey]
			if !ok {
				return errors.New("missing key from *kamajiv1alpha1.TenantControlPlane admin kubeconfig secret")
			}

			capiAdminKubeconfig.SetLabels(labels)

			capiAdminKubeconfig.Data = map[string][]byte{
				"value": value,
			}
			capiAdminKubeconfig.Type = capiv1beta1.ClusterSecretType

			return controllerutil.SetControllerReference(&kcp, capiAdminKubeconfig, r.client.Scheme())
		})

		return scopeErr //nolint:wrapcheck
	})
	if err != nil {
		return errors.Wrap(err, "cannot create or update admin Kubeconfig secret")
	}

	return nil
}
