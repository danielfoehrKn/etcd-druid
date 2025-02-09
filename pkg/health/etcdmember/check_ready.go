// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package etcdmember

import (
	"context"
	"strings"
	"time"

	componentlease "github.com/gardener/etcd-druid/pkg/component/etcd/lease"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"

	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	controllersconfig "github.com/gardener/etcd-druid/controllers/config"
	"github.com/gardener/etcd-druid/pkg/common"
)

type readyCheck struct {
	logger logr.Logger

	memberConfig controllersconfig.EtcdMemberConfig
	cl           client.Client
}

// TimeNow is the function used by this check to get the current time.
var TimeNow = time.Now

func (r *readyCheck) Check(ctx context.Context, etcd druidv1alpha1.Etcd) []Result {
	var (
		results   []Result
		checkTime = TimeNow().UTC()
	)

	leases := &coordinationv1.LeaseList{}
	if err := r.cl.List(ctx, leases, client.InNamespace(etcd.Namespace), client.MatchingLabels{
		common.GardenerOwnedBy: etcd.Name, v1beta1constants.GardenerPurpose: componentlease.PurposeMemberLease}); err != nil {
		r.logger.Error(err, "failed to get leases for etcd member readiness check")
	}

	for _, lease := range leases.Items {
		var (
			id, role = separateIdFromRole(lease.Spec.HolderIdentity)
			res      = &result{
				id:   id,
				name: lease.Name,
				role: role,
			}
		)

		// Check if member is in bootstrapping phase
		// Members are supposed to be added to the members array only if they have joined the cluster (== RenewTime is set).
		// This behavior is expected by the `Ready` condition and it will become imprecise if members are added here too early.
		renew := lease.Spec.RenewTime
		if renew == nil {
			r.logger.Info("Member hasn't acquired lease yet, still in bootstrapping phase", "name", lease.Name)
			continue
		}

		// Check if member state must be considered as not ready
		if renew.Add(r.memberConfig.EtcdMemberUnknownThreshold).Add(r.memberConfig.EtcdMemberNotReadyThreshold).Before(checkTime) {
			res.status = druidv1alpha1.EtcdMemberStatusNotReady
			res.reason = "UnknownGracePeriodExceeded"
			results = append(results, res)
			continue
		}

		// Check if member state must be considered as unknown
		if renew.Add(r.memberConfig.EtcdMemberUnknownThreshold).Before(checkTime) {
			// If pod is not running or cannot be found then we deduce that the status is NotReady.
			ready, err := r.checkContainersAreReady(ctx, lease.Namespace, lease.Name)
			if (err == nil && !ready) || apierrors.IsNotFound(err) {
				res.status = druidv1alpha1.EtcdMemberStatusNotReady
				res.reason = "ContainersNotReady"
				results = append(results, res)
				continue
			}

			res.status = druidv1alpha1.EtcdMemberStatusUnknown
			res.reason = "LeaseExpired"
			results = append(results, res)
			continue
		}

		res.status = druidv1alpha1.EtcdMemberStatusReady
		res.reason = "LeaseSucceeded"
		results = append(results, res)
	}

	return results
}

const holderIdentitySeparator = ":"

func separateIdFromRole(holderIdentity *string) (*string, *druidv1alpha1.EtcdRole) {
	if holderIdentity == nil {
		return nil, nil
	}
	parts := strings.SplitN(*holderIdentity, holderIdentitySeparator, 2)
	id := &parts[0]
	if len(parts) != 2 {
		return id, nil
	}

	switch druidv1alpha1.EtcdRole(parts[1]) {
	case druidv1alpha1.EtcdRoleLeader:
		role := druidv1alpha1.EtcdRoleLeader
		return id, &role
	case druidv1alpha1.EtcdRoleMember:
		role := druidv1alpha1.EtcdRoleMember
		return id, &role
	default:
		return id, nil
	}
}

func (r *readyCheck) checkContainersAreReady(ctx context.Context, namespace string, name string) (bool, error) {
	pod := &corev1.Pod{}
	if err := r.cl.Get(ctx, kutil.Key(namespace, name), pod); err != nil {
		return false, err
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.ContainersReady {
			return cond.Status == corev1.ConditionTrue, nil
		}
	}

	return false, nil
}

// ReadyCheck returns a check for the "Ready" condition.
func ReadyCheck(cl client.Client, logger logr.Logger, config controllersconfig.EtcdCustodianController) Checker {
	return &readyCheck{
		logger: logger,

		cl:           cl,
		memberConfig: config.EtcdMember,
	}
}
