package k8ssandra

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	api "github.com/k8ssandra/k8ssandra-operator/apis/k8ssandra/v1alpha1"
	"github.com/k8ssandra/k8ssandra-operator/pkg/annotations"
	cassandra "github.com/k8ssandra/k8ssandra-operator/pkg/cassandra"
	medusa "github.com/k8ssandra/k8ssandra-operator/pkg/medusa"
	"github.com/k8ssandra/k8ssandra-operator/pkg/result"
	"github.com/k8ssandra/k8ssandra-operator/pkg/secret"
	"github.com/k8ssandra/k8ssandra-operator/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Create all things Medusa related in the cassdc podTemplateSpec
func (r *K8ssandraClusterReconciler) reconcileMedusa(
	ctx context.Context,
	kc *api.K8ssandraCluster,
	dcConfig *cassandra.DatacenterConfig,
	remoteClient client.Client,
	logger logr.Logger,
) result.ReconcileResult {
	namespace := utils.FirstNonEmptyString(dcConfig.Meta.Namespace, kc.Namespace)
	logger.Info("Medusa reconcile for " + dcConfig.Meta.Name + " on namespace " + namespace)
	medusaSpec := kc.Spec.Medusa
	if medusaSpec != nil {
		logger.Info("Medusa is enabled")

		// Check that certificates are provided if client encryption is enabled
		if cassandra.ClientEncryptionEnabled(dcConfig) {
			if kc.Spec.UseExternalSecrets() {
				medusaSpec.CertificatesSecretRef.Name = ""
			} else if medusaSpec.CertificatesSecretRef.Name == "" {
				return result.Error(fmt.Errorf("medusa encryption certificates were not provided despite client encryption being enabled"))
			}
		}
		if medusaSpec.StorageProperties.StorageProvider != "local" && medusaSpec.StorageProperties.StorageSecretRef.Name == "" {
			return result.Error(fmt.Errorf("medusa storage secret is not defined for storage provider %s", medusaSpec.StorageProperties.StorageProvider))
		}
		if res := r.reconcileMedusaConfigMap(ctx, remoteClient, kc, logger, namespace); res.Completed() {
			return res
		}

		medusa.UpdateMedusaInitContainer(dcConfig, medusaSpec, kc.Spec.UseExternalSecrets(), kc.SanitizedName(), logger)
		medusa.UpdateMedusaMainContainer(dcConfig, medusaSpec, kc.Spec.UseExternalSecrets(), kc.SanitizedName(), logger)
		medusa.UpdateMedusaVolumes(dcConfig, medusaSpec, kc.SanitizedName())
		if !kc.Spec.UseExternalSecrets() {
			cassandra.AddCqlUser(medusaSpec.CassandraUserSecretRef, dcConfig, medusa.CassandraUserSecretName(medusaSpec, kc.SanitizedName()))
		}
	} else {
		logger.Info("Medusa is not enabled")
	}

	return result.Continue()
}

// Generate a secret for Medusa or use the existing one if provided in the spec
func (r *K8ssandraClusterReconciler) reconcileMedusaSecrets(
	ctx context.Context,
	kc *api.K8ssandraCluster,
	logger logr.Logger,
) result.ReconcileResult {
	logger.Info("Reconciling Medusa user secrets")
	if kc.Spec.Medusa != nil && !kc.Spec.UseExternalSecrets() {
		cassandraUserSecretRef := kc.Spec.Medusa.CassandraUserSecretRef
		if cassandraUserSecretRef.Name == "" {
			cassandraUserSecretRef.Name = medusa.CassandraUserSecretName(kc.Spec.Medusa, kc.SanitizedName())
		}
		logger = logger.WithValues(
			"MedusaCassandraUserSecretRef",
			cassandraUserSecretRef,
		)
		kcKey := utils.GetKey(kc)
		if err := secret.ReconcileSecret(ctx, r.Client, cassandraUserSecretRef.Name, kcKey); err != nil {
			logger.Error(err, "Failed to reconcile Medusa CQL user secret")
			return result.Error(err)
		}
	}
	logger.Info("Medusa user secrets successfully reconciled")
	return result.Continue()
}

// Create the Medusa config map if it doesn't exist
func (r *K8ssandraClusterReconciler) reconcileMedusaConfigMap(
	ctx context.Context,
	remoteClient client.Client,
	kc *api.K8ssandraCluster,
	logger logr.Logger,
	namespace string,
) result.ReconcileResult {
	logger.Info("Reconciling Medusa configMap on namespace : " + namespace)
	if kc.Spec.Medusa != nil {
		medusaIni := medusa.CreateMedusaIni(kc)
		configMapKey := client.ObjectKey{
			Namespace: kc.Namespace,
			Name:      fmt.Sprintf("%s-medusa", kc.SanitizedName()),
		}

		logger := logger.WithValues("MedusaConfigMap", configMapKey)
		desiredConfigMap := medusa.CreateMedusaConfigMap(namespace, kc.SanitizedName(), medusaIni)
		// Compute a hash which will allow to compare desired and actual configMaps
		annotations.AddHashAnnotation(desiredConfigMap)
		actualConfigMap := &corev1.ConfigMap{}

		if err := remoteClient.Get(ctx, configMapKey, actualConfigMap); err != nil {
			if errors.IsNotFound(err) {
				if err := remoteClient.Create(ctx, desiredConfigMap); err != nil {
					logger.Error(err, "Failed to create Medusa ConfigMap")
					return result.Error(err)
				}
			}
		}

		actualConfigMap = actualConfigMap.DeepCopy()

		if !annotations.CompareHashAnnotations(actualConfigMap, desiredConfigMap) {
			logger.Info("Updating configMap on namespace " + actualConfigMap.ObjectMeta.Namespace)
			resourceVersion := actualConfigMap.GetResourceVersion()
			desiredConfigMap.DeepCopyInto(actualConfigMap)
			actualConfigMap.SetResourceVersion(resourceVersion)
			if err := remoteClient.Update(ctx, actualConfigMap); err != nil {
				logger.Error(err, "Failed to update Medusa ConfigMap resource")
				return result.Error(err)
			}
			return result.RequeueSoon(r.DefaultDelay)
		}
	}
	logger.Info("Medusa ConfigMap successfully reconciled")
	return result.Continue()
}
