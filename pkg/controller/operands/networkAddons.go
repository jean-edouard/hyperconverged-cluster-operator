package operands

import (
	"errors"
	"reflect"

	networkaddonsshared "github.com/kubevirt/cluster-network-addons-operator/pkg/apis/networkaddonsoperator/shared"
	networkaddonsv1 "github.com/kubevirt/cluster-network-addons-operator/pkg/apis/networkaddonsoperator/v1"
	networkaddonsnames "github.com/kubevirt/cluster-network-addons-operator/pkg/names"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sdkapi "kubevirt.io/controller-lifecycle-operator-sdk/pkg/sdk/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hcov1beta1 "github.com/kubevirt/hyperconverged-cluster-operator/pkg/apis/hco/v1beta1"
	"github.com/kubevirt/hyperconverged-cluster-operator/pkg/controller/common"
	"github.com/kubevirt/hyperconverged-cluster-operator/pkg/util"
	hcoutil "github.com/kubevirt/hyperconverged-cluster-operator/pkg/util"
)

type cnaHandler genericOperand

func newCnaHandler(Client client.Client, Scheme *runtime.Scheme) *cnaHandler {
	return &cnaHandler{
		Client: Client,
		Scheme: Scheme,
		crType: "NetworkAddonsConfig",
		// Previous versions used to have HCO-operator (scope namespace)
		// as the owner of NetworkAddons (scope cluster).
		// It's not legal, so remove that.
		removeExistingOwner: true,
		hooks:               &cnaHooks{},
	}
}

type cnaHooks struct {
	cache *networkaddonsv1.NetworkAddonsConfig
}

func (h *cnaHooks) getFullCr(hc *hcov1beta1.HyperConverged) (client.Object, error) {
	if h.cache == nil {
		cna, err := NewNetworkAddons(hc)
		if err != nil {
			return nil, err
		}
		h.cache = cna
	}
	return h.cache, nil
}

func (h cnaHooks) getEmptyCr() client.Object                          { return &networkaddonsv1.NetworkAddonsConfig{} }
func (h cnaHooks) postFound(*common.HcoRequest, runtime.Object) error { return nil }
func (h cnaHooks) getConditions(cr runtime.Object) []conditionsv1.Condition {
	return cr.(*networkaddonsv1.NetworkAddonsConfig).Status.Conditions
}
func (h cnaHooks) checkComponentVersion(cr runtime.Object) bool {
	found := cr.(*networkaddonsv1.NetworkAddonsConfig)
	return checkComponentVersion(hcoutil.CnaoVersionEnvV, found.Status.ObservedVersion)
}
func (h cnaHooks) getObjectMeta(cr runtime.Object) *metav1.ObjectMeta {
	return &cr.(*networkaddonsv1.NetworkAddonsConfig).ObjectMeta
}
func (h *cnaHooks) reset() {
	h.cache = nil
}

func (h *cnaHooks) updateCr(req *common.HcoRequest, Client client.Client, exists runtime.Object, required runtime.Object) (bool, bool, error) {
	networkAddons, ok1 := required.(*networkaddonsv1.NetworkAddonsConfig)
	found, ok2 := exists.(*networkaddonsv1.NetworkAddonsConfig)

	if !ok1 || !ok2 {
		return false, false, errors.New("can't convert to CNA")
	}

	h.setDeployOvsAnnotation(req, found)

	changed := h.updateSpec(req, found, networkAddons)
	changed = h.updateLabels(found, networkAddons) || changed

	if changed {
		return h.updateCnaCr(req, Client, found)
	}

	return false, false, nil
}

func (h *cnaHooks) updateCnaCr(req *common.HcoRequest, Client client.Client, found *networkaddonsv1.NetworkAddonsConfig) (bool, bool, error) {
	err := Client.Update(req.Ctx, found)
	if err != nil {
		return false, false, err
	}
	return true, !req.HCOTriggered, nil
}

func (h *cnaHooks) updateLabels(found *networkaddonsv1.NetworkAddonsConfig, networkAddons *networkaddonsv1.NetworkAddonsConfig) bool {
	if !reflect.DeepEqual(found.Labels, networkAddons.Labels) {
		util.DeepCopyLabels(&networkAddons.ObjectMeta, &found.ObjectMeta)
		return true
	}
	return false
}

func (h *cnaHooks) updateSpec(req *common.HcoRequest, found *networkaddonsv1.NetworkAddonsConfig, networkAddons *networkaddonsv1.NetworkAddonsConfig) bool {
	if !reflect.DeepEqual(found.Spec, networkAddons.Spec) && !req.UpgradeMode {
		if req.HCOTriggered {
			req.Logger.Info("Updating existing Network Addons's Spec to new opinionated values")
		} else {
			req.Logger.Info("Reconciling an externally updated Network Addons's Spec to its opinionated values")
		}
		networkAddons.Spec.DeepCopyInto(&found.Spec)
		return true
	}
	return false
}

// If deployOVS annotation doesn't exists prior the upgrade - set this annotation to true;
// Otherwise - remain the value as it is.
func (h *cnaHooks) setDeployOvsAnnotation(req *common.HcoRequest, found *networkaddonsv1.NetworkAddonsConfig) {
	if req.UpgradeMode {
		_, exists := req.Instance.Annotations["deployOVS"]
		if !exists {
			if req.Instance.Annotations == nil {
				req.Instance.Annotations = map[string]string{}
			}
			if found.Spec.Ovs != nil {
				req.Instance.Annotations["deployOVS"] = "true"
				req.Logger.Info("deployOVS annotation is set to true.")
			} else {
				req.Instance.Annotations["deployOVS"] = "false"
				req.Logger.Info("deployOVS annotation is set to false.")
			}

			req.Dirty = true
		}
	}
}

func NewNetworkAddons(hc *hcov1beta1.HyperConverged, opts ...string) (*networkaddonsv1.NetworkAddonsConfig, error) {

	cnaoSpec := networkaddonsshared.NetworkAddonsConfigSpec{
		Multus:      &networkaddonsshared.Multus{},
		LinuxBridge: &networkaddonsshared.LinuxBridge{},
		NMState:     &networkaddonsshared.NMState{},
		KubeMacPool: &networkaddonsshared.KubeMacPool{},
	}

	cnaoSpec.Ovs = hcoAnnotation2CnaoSpec(hc.ObjectMeta.Annotations)
	cnaoInfra := hcoConfig2CnaoPlacement(hc.Spec.Infra.NodePlacement)
	cnaoWorkloads := hcoConfig2CnaoPlacement(hc.Spec.Workloads.NodePlacement)
	if cnaoInfra != nil || cnaoWorkloads != nil {
		cnaoSpec.PlacementConfiguration = &networkaddonsshared.PlacementConfiguration{
			Infra:     cnaoInfra,
			Workloads: cnaoWorkloads,
		}
	}

	// TODO: support passing certificate rotation configuration to CNAO spec

	cna := NewNetworkAddonsWithNameOnly(hc, opts...)
	cna.Spec = cnaoSpec

	if err := applyPatchToSpec(hc, common.JSONPatchCNAOAnnotationName, cna); err != nil {
		return nil, err
	}

	return cna, nil
}

func NewNetworkAddonsWithNameOnly(hc *hcov1beta1.HyperConverged, opts ...string) *networkaddonsv1.NetworkAddonsConfig {
	return &networkaddonsv1.NetworkAddonsConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkaddonsnames.OPERATOR_CONFIG,
			Labels:    getLabels(hc, hcoutil.AppComponentNetwork),
			Namespace: getNamespace(hcoutil.UndefinedNamespace, opts),
		},
	}
}

func hcoConfig2CnaoPlacement(hcoConf *sdkapi.NodePlacement) *networkaddonsshared.Placement {
	if hcoConf == nil {
		return nil
	}
	empty := true
	cnaoPlacement := &networkaddonsshared.Placement{}
	if hcoConf.Affinity != nil {
		empty = false
		hcoConf.Affinity.DeepCopyInto(&cnaoPlacement.Affinity)
	}

	for _, hcoTol := range hcoConf.Tolerations {
		empty = false
		cnaoTol := corev1.Toleration{}
		hcoTol.DeepCopyInto(&cnaoTol)
		cnaoPlacement.Tolerations = append(cnaoPlacement.Tolerations, cnaoTol)
	}

	if len(hcoConf.NodeSelector) > 0 {
		empty = false
		cnaoPlacement.NodeSelector = make(map[string]string)
		for k, v := range hcoConf.NodeSelector {
			cnaoPlacement.NodeSelector[k] = v
		}
	}

	if empty {
		return nil
	}
	return cnaoPlacement
}

func hcoAnnotation2CnaoSpec(hcoAnnotations map[string]string) *networkaddonsshared.Ovs {
	val, exists := hcoAnnotations["deployOVS"]
	if exists && val == "true" {
		return &networkaddonsshared.Ovs{}
	}
	return nil
}
