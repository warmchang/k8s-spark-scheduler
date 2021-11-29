// Copyright (c) 2019 Palantir Technologies. All rights reserved.
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

package resourcereservationmigrator

import (
	"context"
	"fmt"
	"github.com/palantir/pkg/retry"

	"github.com/palantir/k8s-spark-scheduler-lib/pkg/apis/sparkscheduler/v1beta2"
	sparkschedulerclient "github.com/palantir/k8s-spark-scheduler-lib/pkg/client/clientset/versioned/typed/sparkscheduler/v1beta2"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/wapp"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	migrationStatusFieldName  = "migrationStatus"
	migrationStatusInProgress = "IN_PROGRESS"
	migrationStatusFinished   = "FINISHED"
)

//ResourceReservationMigrator Manages the migration of Resource Reservation objects from storage version v1beta1 to v1beta2
type ResourceReservationMigrator struct {
	apiextensionsclientset        apiextensionsclientset.Interface
	resourceReservationKubeClient sparkschedulerclient.SparkschedulerV1beta2Interface
	resourceReservationCRDName    string
}

//New Returns a new ResourceReservationMigrator
func New(
	apiextensionsclientset apiextensionsclientset.Interface,
	resourceReservationKubeClient sparkschedulerclient.SparkschedulerV1beta2Interface,
	resourceReservationCRDName string,
) *ResourceReservationMigrator {
	return &ResourceReservationMigrator{
		apiextensionsclientset,
		resourceReservationKubeClient,
		resourceReservationCRDName,
	}
}

func (rrm *ResourceReservationMigrator) getRRV1Beta2CRD(ctx context.Context) (*v1.CustomResourceDefinition, error) {
	return rrm.apiextensionsclientset.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, rrm.resourceReservationCRDName, metav1.GetOptions{})
}

//RunMigration runs the migration of stored Resource Reservation objects from v1beta1 to v1beta2 if it has
//not already been run. If it has already run, it does nothing.
//
//The migration is performed by iterating through each of the Resource Reservations and updating them with the same content as per
//https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/#upgrade-existing-objects-to-a-new-stored-version
//
//We record that a migration has successfully completed by writing a `migrationStatus` field to the annotations of the CRD
//of the v1beta2 ResourceReservation CRD. The field has the values `IN_PROGRESS` and `FINISHED`. We then use this field
//to determine whether the migration should be run again.
func (rrm *ResourceReservationMigrator) RunMigration(ctx context.Context) {
	go func() {
		// We explicitly do not want to stop scheduler from running if the migration fails
		_ = wapp.RunWithFatalLogging(ctx, rrm.maybeRunMigration)
	}()
}

func (rrm *ResourceReservationMigrator) maybeRunMigration(ctx context.Context) error {
	ran, err := rrm.hasMigrationAlreadyRan(ctx)
	if err != nil {
		return err
	}
	if ran {
		return nil
	}
	return rrm.runMigration(ctx)
}

func (rrm *ResourceReservationMigrator) hasMigrationAlreadyRan(ctx context.Context) (bool, error) {
	// If the `migrationStatus` field does not exist then the migration has never been run
	// If the `migrationStatus` field is `IN_PROGRESS` another node may be running the migration or we may have been
	// killed while we were processing the last migration. In either case we will run the migration again as it is safe
	// to run the migration many times
	crd, err := rrm.getRRV1Beta2CRD(ctx)
	if err != nil {
		return false, err
	}

	if migrationStatus, ok := crd.ObjectMeta.Annotations[migrationStatusFieldName]; ok {
		if migrationStatus == migrationStatusFinished {
			return true, nil
		}
	}
	return false, nil
}

func (rrm *ResourceReservationMigrator) runMigration(ctx context.Context) error {
	// As per https://github.com/kubernetes/client-go/issues/159#issuecomment-288624475, namespace = "" lists the resource across
	// all namespaces
	resourceReservations, err := rrm.resourceReservationKubeClient.ResourceReservations("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return werror.Wrap(err, "Failed to list all resource reservations")
	}
	if err = rrm.setMigrationAsStartedIfItHasNotBeenStartedBefore(ctx); err != nil {
		return err
	}

	for _, resourceReservation := range resourceReservations.Items {
		if err = rrm.migrateResourceReservation(ctx, &resourceReservation); err != nil {
			return err
		}
	}
	return rrm.patchMigration(ctx, migrationStatusFinished)
}

func (rrm *ResourceReservationMigrator) migrateResourceReservation(ctx context.Context, resourceReservation *v1beta2.ResourceReservation) error {
	// PATCH is fine in this scenario as described here https://github.com/kubernetes-sigs/kube-storage-version-migrator/issues/65#issuecomment-704480927
	// We use patch in order to handle the following edgecase if we use UPDATE:
	// 1. We get the resource reservation R1 in the migration code
	// 2. The scheduler code updates the reservation through normal operation
	// 3. We update R1 with the resource reservation we got before the update, resulting in a collision or a dirty write
	// By applying an empty patch we will never hit this scenario
	return retry.Do(ctx, func() error {
		_, err := rrm.resourceReservationKubeClient.ResourceReservations(resourceReservation.Namespace).Patch(ctx, resourceReservation.Name, types.StrategicMergePatchType, []byte(""), metav1.PatchOptions{})
		return err
	})
}

func (rrm *ResourceReservationMigrator) setMigrationAsStartedIfItHasNotBeenStartedBefore(ctx context.Context) error {
	return retry.Do(ctx, func() error {
		crd, err := rrm.getRRV1Beta2CRD(ctx)
		if err != nil {
			return err
		}
		if _, ok := crd.ObjectMeta.Annotations[migrationStatusFieldName]; ok {
			// We only want to modify the migration status if it has not been set
			return rrm.updateMigration(ctx, crd, migrationStatusInProgress)
		}
		return nil
	})
}

func (rrm *ResourceReservationMigrator) updateMigration(ctx context.Context, crd *v1.CustomResourceDefinition, status string) error {
	crd.ObjectMeta.Annotations[migrationStatusFieldName] = status
	_, err := rrm.apiextensionsclientset.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, crd, metav1.UpdateOptions{})
	return err
}

func (rrm *ResourceReservationMigrator) patchMigration(ctx context.Context, status string) error {
	_, err := rrm.apiextensionsclientset.ApiextensionsV1().CustomResourceDefinitions().Patch(ctx, rrm.resourceReservationCRDName, types.StrategicMergePatchType, generateMigrationStatusPatch(status), metav1.PatchOptions{})
	return err
}

func generateMigrationStatusPatch(status string) []byte {
	return []byte(fmt.Sprintf(`{
		"metadata": {
			"annotations": {
				"%s": "%s"
			}
		}
	}`, migrationStatusFieldName, status))
}
