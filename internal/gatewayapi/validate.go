// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package gatewayapi

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gwapiv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/envoyproxy/gateway/internal/gatewayapi/resource"
	"github.com/envoyproxy/gateway/internal/gatewayapi/status"
)

func (t *Translator) validateBackendRef(backendRefContext BackendRefContext, route RouteContext,
	resources *resource.Resources, backendNamespace string, routeKind gwapiv1.Kind,
) status.Error {
	backendRef := GetBackendRef(backendRefContext)

	if err := t.validateBackendRefFilters(backendRefContext, routeKind); err != nil {
		return err
	}
	if err := t.validateBackendRefGroup(backendRef); err != nil {
		return err
	}
	if err := t.validateBackendRefKind(backendRef); err != nil {
		return err
	}
	if err := t.validateBackendNamespace(backendRef, route, resources, routeKind); err != nil {
		return err
	}
	if err := t.validateBackendPort(backendRef); err != nil {
		return err
	}

	protocol := corev1.ProtocolTCP
	if routeKind == resource.KindUDPRoute {
		protocol = corev1.ProtocolUDP
	}

	backendRefKind := KindDerefOr(backendRef.Kind, resource.KindService)
	switch backendRefKind {
	case resource.KindService:
		if err := validateBackendRefService(backendRef.BackendObjectReference, resources, backendNamespace, protocol); err != nil {
			return err
		}
	case resource.KindServiceImport:
		if err := t.validateBackendServiceImport(backendRef.BackendObjectReference, resources, backendNamespace, protocol); err != nil {
			return err
		}
	case egv1a1.KindBackend:
		if err := t.validateBackendRefBackend(backendRef.BackendObjectReference, resources, backendNamespace, false); err != nil {
			return err
		}
	}
	return nil
}

func (t *Translator) validateBackendRefGroup(backendRef *gwapiv1a2.BackendRef) status.Error {
	if backendRef.Group != nil && *backendRef.Group != "" && *backendRef.Group != GroupMultiClusterService && *backendRef.Group != egv1a1.GroupName {
		return status.NewRouteStatusError(
			fmt.Errorf("Group is invalid, only the core API group (specified by omitting the group field or setting it to an empty string), %s and %s are supported",
				GroupMultiClusterService,
				egv1a1.GroupName),
			gwapiv1.RouteReasonInvalidKind)
	}
	return nil
}

func (t *Translator) validateBackendRefKind(backendRef *gwapiv1a2.BackendRef) status.Error {
	if backendRef.Kind != nil && *backendRef.Kind != resource.KindService && *backendRef.Kind != resource.KindServiceImport && *backendRef.Kind != egv1a1.KindBackend {
		return status.NewRouteStatusError(
			fmt.Errorf("Kind is invalid, only Service, MCS ServiceImport and Envoy Gateway Backend are supported"),
			gwapiv1.RouteReasonInvalidKind)
	}
	return nil
}

func (t *Translator) validateBackendRefFilters(backendRef BackendRefContext, routeKind gwapiv1.Kind) status.Error {
	filters := GetFilters(backendRef)
	var unsupportedFilters bool

	switch routeKind {
	case resource.KindHTTPRoute:
		for _, filter := range filters.([]gwapiv1.HTTPRouteFilter) {
			// Reuse the same validation logic as HTTPRoute to validate the ExtensionRef
			if err := ValidateHTTPRouteFilter(&filter, t.ExtensionGroupKinds...); err != nil {
				unsupportedFilters = true
				continue
			}
			if filter.Type != gwapiv1.HTTPRouteFilterRequestHeaderModifier &&
				filter.Type != gwapiv1.HTTPRouteFilterResponseHeaderModifier &&
				filter.Type != gwapiv1.HTTPRouteFilterExtensionRef {
				unsupportedFilters = true
			}
		}
	case resource.KindGRPCRoute:
		for _, filter := range filters.([]gwapiv1.GRPCRouteFilter) {
			if filter.Type != gwapiv1.GRPCRouteFilterRequestHeaderModifier &&
				filter.Type != gwapiv1.GRPCRouteFilterResponseHeaderModifier {
				unsupportedFilters = true
			}
		}
	default:
		return nil
	}

	if unsupportedFilters {
		message := "Specific filter is not supported within BackendRef, only RequestHeaderModifier, ResponseHeaderModifier and gateway.envoyproxy.io/HTTPRouteFilter are supported"
		if routeKind == resource.KindGRPCRoute {
			message = "Specific filter is not supported within BackendRef, only RequestHeaderModifier and ResponseHeaderModifier are supported"
		}
		return status.NewRouteStatusError(
			errors.New(message),
			status.RouteReasonUnsupportedRefValue)
	}

	return nil
}

func (t *Translator) validateBackendNamespace(backendRef *gwapiv1a2.BackendRef, route RouteContext,
	resources *resource.Resources, routeKind gwapiv1.Kind,
) status.Error {
	if backendRef.Namespace != nil && string(*backendRef.Namespace) != "" && string(*backendRef.Namespace) != route.GetNamespace() {
		if !t.validateCrossNamespaceRef(
			crossNamespaceFrom{
				group:     gwapiv1.GroupName,
				kind:      string(routeKind),
				namespace: route.GetNamespace(),
			},
			crossNamespaceTo{
				group:     GroupDerefOr(backendRef.Group, ""),
				kind:      KindDerefOr(backendRef.Kind, resource.KindService),
				namespace: string(*backendRef.Namespace),
				name:      string(backendRef.Name),
			},
			resources.ReferenceGrants,
		) {
			return status.NewRouteStatusError(
				fmt.Errorf("Backend ref to %s %s/%s not permitted by any ReferenceGrant.",
					KindDerefOr(backendRef.Kind, resource.KindService),
					*backendRef.Namespace,
					backendRef.Name),
				gwapiv1.RouteReasonRefNotPermitted)
		}
	}
	return nil
}

func (t *Translator) validateBackendPort(backendRef *gwapiv1a2.BackendRef) status.Error {
	if backendRef == nil {
		return nil
	}

	if KindDerefOr(backendRef.Kind, resource.KindService) == egv1a1.KindBackend {
		return nil
	}

	if backendRef.Port == nil {
		return status.NewRouteStatusError(
			errors.New("A valid port number corresponding to a port on the Service must be specified"),
			status.RouteReasonPortNotSpecified)
	}
	return nil
}

func validateBackendRefService(backendRef gwapiv1a2.BackendObjectReference, resources *resource.Resources,
	serviceNamespace string, protocol corev1.Protocol,
) status.Error {
	service := resources.GetService(serviceNamespace, string(backendRef.Name))
	if service == nil {
		return status.NewRouteStatusError(
			fmt.Errorf("service %s/%s not found", serviceNamespace, string(backendRef.Name)),
			gwapiv1.RouteReasonBackendNotFound)
	}
	var portFound bool
	for _, port := range service.Spec.Ports {
		portProtocol := port.Protocol
		if port.Protocol == "" { // Default protocol is TCP
			portProtocol = corev1.ProtocolTCP
		}
		if port.Port == int32(*backendRef.Port) && portProtocol == protocol {
			portFound = true
			break
		}
	}

	if !portFound {
		return status.NewRouteStatusError(
			fmt.Errorf("%s Port %d not found on Service %s/%s", string(protocol), *backendRef.Port, serviceNamespace, string(backendRef.Name)),
			status.RouteReasonPortNotFound)
	}
	return nil
}

func (t *Translator) validateBackendServiceImport(backendRef gwapiv1a2.BackendObjectReference, resources *resource.Resources,
	serviceImportNamespace string, protocol corev1.Protocol,
) status.Error {
	serviceImport := resources.GetServiceImport(serviceImportNamespace, string(backendRef.Name))
	if serviceImport == nil {
		return status.NewRouteStatusError(
			fmt.Errorf("service import %s/%s not found", serviceImportNamespace, backendRef.Name),
			gwapiv1.RouteReasonBackendNotFound)
	}

	var portFound bool
	for _, port := range serviceImport.Spec.Ports {
		portProtocol := port.Protocol
		if port.Protocol == "" { // Default protocol is TCP
			portProtocol = corev1.ProtocolTCP
		}
		if port.Port == int32(*backendRef.Port) && portProtocol == protocol {
			portFound = true
			break
		}
	}

	if !portFound {
		return status.NewRouteStatusError(
			fmt.Errorf("%s port %d not found on service import %s/%s", string(protocol), *backendRef.Port, serviceImportNamespace, backendRef.Name),
			status.RouteReasonPortNotFound)
	}

	return nil
}

func (t *Translator) validateBackendRefBackend(backendRef gwapiv1a2.BackendObjectReference, resources *resource.Resources,
	backendNamespace string, allowUDS bool,
) status.Error {
	if !t.BackendEnabled {
		return status.NewRouteStatusError(
			errors.New("Backend is disabled in Envoy Gateway configuration"),
			gwapiv1.RouteReasonUnsupportedValue,
		)
	}

	backend := resources.GetBackend(backendNamespace, string(backendRef.Name))
	if backend == nil {
		return status.NewRouteStatusError(
			fmt.Errorf("Backend %s/%s not found", backendNamespace, backendRef.Name),
			gwapiv1.RouteReasonBackendNotFound,
		)
	}

	if err := validateBackend(backend, resources.BackendTLSPolicies); err != nil {
		return err
	}

	for _, bep := range backend.Spec.Endpoints {
		if bep.Unix != nil && !allowUDS {
			return status.NewRouteStatusError(
				errors.New("unix domain sockets are not supported in backend references"),
				status.RouteReasonUnsupportedAddressType,
			)
		}
	}

	return nil
}

func (t *Translator) validateListenerConditions(listener *ListenerContext) (isReady bool) {
	lConditions := listener.GetConditions()
	if len(lConditions) == 0 {
		status.SetGatewayListenerStatusCondition(listener.gateway.Gateway, listener.listenerStatusIdx,
			gwapiv1.ListenerConditionProgrammed, metav1.ConditionTrue, gwapiv1.ListenerReasonProgrammed,
			"Sending translated listener configuration to the data plane")
		status.SetGatewayListenerStatusCondition(listener.gateway.Gateway, listener.listenerStatusIdx,
			gwapiv1.ListenerConditionAccepted, metav1.ConditionTrue, gwapiv1.ListenerReasonAccepted,
			"Listener has been successfully translated")
		status.SetGatewayListenerStatusCondition(listener.gateway.Gateway, listener.listenerStatusIdx,
			gwapiv1.ListenerConditionResolvedRefs, metav1.ConditionTrue, gwapiv1.ListenerReasonResolvedRefs,
			"Listener references have been resolved")
		return true
	}

	// Any condition on the listener apart from Programmed=true indicates an error.
	if lConditions[0].Type != string(gwapiv1.ListenerConditionProgrammed) || lConditions[0].Status != metav1.ConditionTrue {
		hasProgrammedCond := false
		hasRefsCond := false
		for _, existing := range lConditions {
			if existing.Type == string(gwapiv1.ListenerConditionProgrammed) {
				hasProgrammedCond = true
			}
			if existing.Type == string(gwapiv1.ListenerConditionResolvedRefs) {
				hasRefsCond = true
			}
		}
		// set "Programmed: false" if it's not set already.
		if !hasProgrammedCond {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalid,
				"Listener is invalid, see other Conditions for details.",
			)
		}
		// set "ResolvedRefs: true" if it's not set already.
		if !hasRefsCond {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionTrue,
				gwapiv1.ListenerReasonResolvedRefs,
				"Listener references have been resolved",
			)
		}
		// skip computing IR
		return false
	}
	return true
}

func (t *Translator) validateAllowedNamespaces(listener *ListenerContext) {
	if listener.AllowedRoutes != nil &&
		listener.AllowedRoutes.Namespaces != nil &&
		listener.AllowedRoutes.Namespaces.From != nil &&
		*listener.AllowedRoutes.Namespaces.From == gwapiv1.NamespacesFromSelector {
		if listener.AllowedRoutes.Namespaces.Selector == nil {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalid,
				"The allowedRoutes.namespaces.selector field must be specified when allowedRoutes.namespaces.from is set to \"Selector\".",
			)
		} else {
			selector, err := metav1.LabelSelectorAsSelector(listener.AllowedRoutes.Namespaces.Selector)
			if err != nil {
				status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
					listener.listenerStatusIdx,
					gwapiv1.ListenerConditionProgrammed,
					metav1.ConditionFalse,
					gwapiv1.ListenerReasonInvalid,
					fmt.Sprintf("The allowedRoutes.namespaces.selector could not be parsed: %v.", err),
				)
			}

			listener.namespaceSelector = selector
		}
	}
}

func (t *Translator) validateTerminateModeAndGetTLSSecrets(listener *ListenerContext, resources *resource.Resources) ([]*corev1.Secret, []*x509.Certificate) {
	if len(listener.TLS.CertificateRefs) == 0 {
		status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
			listener.listenerStatusIdx,
			gwapiv1.ListenerConditionProgrammed,
			metav1.ConditionFalse,
			gwapiv1.ListenerReasonInvalid,
			"Listener must have at least 1 TLS certificate ref",
		)
		return nil, nil
	}

	secrets := make([]*corev1.Secret, 0)
	for _, certificateRef := range listener.TLS.CertificateRefs {
		// TODO zhaohuabing: reuse validateSecretRef
		if certificateRef.Group != nil && string(*certificateRef.Group) != "" {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalidCertificateRef,
				"Listener's TLS certificate ref group must be unspecified/empty.",
			)
			break
		}

		if certificateRef.Kind != nil && string(*certificateRef.Kind) != resource.KindSecret {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalidCertificateRef,
				fmt.Sprintf("Listener's TLS certificate ref kind must be %s.", resource.KindSecret),
			)
			break
		}

		secretNamespace := listener.gateway.Namespace

		if certificateRef.Namespace != nil && string(*certificateRef.Namespace) != "" && string(*certificateRef.Namespace) != listener.gateway.Namespace {
			if !t.validateCrossNamespaceRef(
				crossNamespaceFrom{
					group:     gwapiv1.GroupName,
					kind:      resource.KindGateway,
					namespace: listener.gateway.Namespace,
				},
				crossNamespaceTo{
					group:     "",
					kind:      resource.KindSecret,
					namespace: string(*certificateRef.Namespace),
					name:      string(certificateRef.Name),
				},
				resources.ReferenceGrants,
			) {
				status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
					listener.listenerStatusIdx,
					gwapiv1.ListenerConditionResolvedRefs,
					metav1.ConditionFalse,
					gwapiv1.ListenerReasonRefNotPermitted,
					fmt.Sprintf("Certificate ref to secret %s/%s not permitted by any ReferenceGrant.", *certificateRef.Namespace, certificateRef.Name),
				)
				break
			}

			secretNamespace = string(*certificateRef.Namespace)
		}

		secret := resources.GetSecret(secretNamespace, string(certificateRef.Name))

		if secret == nil {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalidCertificateRef,
				fmt.Sprintf("Secret %s/%s does not exist.", listener.gateway.Namespace, certificateRef.Name),
			)
			break
		}

		if secret.Type != corev1.SecretTypeTLS {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalidCertificateRef,
				fmt.Sprintf("Secret %s/%s must be of type %s.", listener.gateway.Namespace, certificateRef.Name, corev1.SecretTypeTLS),
			)
			break
		}

		if len(secret.Data[corev1.TLSCertKey]) == 0 || len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalidCertificateRef,
				fmt.Sprintf("Secret %s/%s must contain %s and %s.", listener.gateway.Namespace, certificateRef.Name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey),
			)
			break
		}

		secrets = append(secrets, secret)
	}

	certs, err := validateTLSSecretsData(secrets, listener.Hostname)
	if err != nil {
		status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
			listener.listenerStatusIdx,
			gwapiv1.ListenerConditionResolvedRefs,
			metav1.ConditionFalse,
			gwapiv1.ListenerReasonInvalidCertificateRef,
			fmt.Sprintf("Secret %s.", err.Error()),
		)
	}

	return secrets, certs
}

func (t *Translator) validateTLSConfiguration(listener *ListenerContext, resources *resource.Resources) {
	switch listener.Protocol {
	case gwapiv1.HTTPProtocolType, gwapiv1.UDPProtocolType, gwapiv1.TCPProtocolType:
		if listener.TLS != nil {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalid,
				fmt.Sprintf("Listener must not have TLS set when protocol is %s.", listener.Protocol),
			)
		}
	case gwapiv1.HTTPSProtocolType:
		if listener.TLS == nil {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalid,
				fmt.Sprintf("Listener must have TLS set when protocol is %s.", listener.Protocol),
			)
			break
		}

		if listener.TLS.Mode != nil && *listener.TLS.Mode != gwapiv1.TLSModeTerminate {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				"UnsupportedTLSMode",
				fmt.Sprintf("TLS %s mode is not supported, TLS mode must be Terminate.", *listener.TLS.Mode),
			)
			break
		}

		secrets, certs := t.validateTerminateModeAndGetTLSSecrets(listener, resources)
		listener.SetTLSSecrets(secrets)

		listener.certDNSNames = make([]string, 0)
		for _, cert := range certs {
			listener.certDNSNames = append(listener.certDNSNames, cert.DNSNames...)
		}

	case gwapiv1.TLSProtocolType:
		if listener.TLS == nil {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalid,
				fmt.Sprintf("Listener must have TLS set when protocol is %s.", listener.Protocol),
			)
			break
		}

		if listener.TLS.Mode != nil && *listener.TLS.Mode == gwapiv1.TLSModePassthrough {
			if len(listener.TLS.CertificateRefs) > 0 {
				status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
					listener.listenerStatusIdx,
					gwapiv1.ListenerConditionProgrammed,
					metav1.ConditionFalse,
					gwapiv1.ListenerReasonInvalid,
					"Listener must not have TLS certificate refs set for TLS mode Passthrough.",
				)
				break
			}
		}

		if listener.TLS.Mode != nil && *listener.TLS.Mode == gwapiv1.TLSModeTerminate {
			if len(listener.TLS.CertificateRefs) == 0 {
				status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
					listener.listenerStatusIdx,
					gwapiv1.ListenerConditionProgrammed,
					metav1.ConditionFalse,
					gwapiv1.ListenerReasonInvalid,
					"Listener must have TLS certificate refs set for TLS mode Terminate.",
				)
				break
			}
			secrets, _ := t.validateTerminateModeAndGetTLSSecrets(listener, resources)
			listener.SetTLSSecrets(secrets)
		}
	}
}

func (t *Translator) validateHostName(listener *ListenerContext) {
	if listener.Protocol == gwapiv1.UDPProtocolType || listener.Protocol == gwapiv1.TCPProtocolType {
		if listener.Hostname != nil {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionProgrammed,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalid,
				fmt.Sprintf("Listener must not have hostname set when protocol is %s.", listener.Protocol),
			)
		}
	}
}

func (t *Translator) validateAllowedRoutes(listener *ListenerContext, routeKinds ...gwapiv1.Kind) {
	canSupportKinds := make([]gwapiv1.RouteGroupKind, len(routeKinds))
	for i, routeKind := range routeKinds {
		canSupportKinds[i] = gwapiv1.RouteGroupKind{Group: GroupPtr(gwapiv1.GroupName), Kind: routeKind}
	}
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		listener.SetSupportedKinds(canSupportKinds...)
		return
	}

	supportedRouteKinds := make([]gwapiv1.Kind, 0)
	supportedKinds := make([]gwapiv1.RouteGroupKind, 0)
	unSupportedKinds := make([]gwapiv1.RouteGroupKind, 0)

	for _, kind := range listener.AllowedRoutes.Kinds {

		// if there is a group it must match `gateway.networking.k8s.io`
		if kind.Group != nil && string(*kind.Group) != gwapiv1.GroupName {
			status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
				listener.listenerStatusIdx,
				gwapiv1.ListenerConditionResolvedRefs,
				metav1.ConditionFalse,
				gwapiv1.ListenerReasonInvalidRouteKinds,
				fmt.Sprintf("Group is not supported, group must be %s", gwapiv1.GroupName),
			)
			continue
		}

		found := false
		for _, routeKind := range routeKinds {
			if kind.Kind == routeKind {
				supportedKinds = append(supportedKinds, kind)
				supportedRouteKinds = append(supportedRouteKinds, kind.Kind)
				found = true
				break
			}
		}

		if !found {
			unSupportedKinds = append(unSupportedKinds, kind)
		}
	}

	for _, kind := range unSupportedKinds {
		var printRouteKinds []gwapiv1.Kind
		if len(supportedKinds) == 0 {
			printRouteKinds = routeKinds
		} else {
			printRouteKinds = supportedRouteKinds
		}
		status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
			listener.listenerStatusIdx,
			gwapiv1.ListenerConditionResolvedRefs,
			metav1.ConditionFalse,
			gwapiv1.ListenerReasonInvalidRouteKinds,
			fmt.Sprintf("%s is not supported, kind must be one of %v", string(kind.Kind), printRouteKinds),
		)
	}

	listener.SetSupportedKinds(supportedKinds...)
}

type portListeners struct {
	listeners []*ListenerContext
	protocols sets.Set[string]
	hostnames map[string]int
}

// Port, protocol and hostname tuple should be unique across all listeners on merged Gateways.
func (t *Translator) validateConflictedMergedListeners(gateways []*GatewayContext) {
	listenerSets := sets.Set[string]{}
	for _, gateway := range gateways {
		for _, listener := range gateway.listeners {
			hostname := new(gwapiv1.Hostname)
			if listener.Hostname != nil {
				hostname = listener.Hostname
			}
			portProtocolHostname := fmt.Sprintf("%s:%s:%d", listener.Protocol, *hostname, listener.Port)
			if listenerSets.Has(portProtocolHostname) {
				status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
					listener.listenerStatusIdx,
					gwapiv1.ListenerConditionConflicted,
					metav1.ConditionTrue,
					gwapiv1.ListenerReasonHostnameConflict,
					"Port, protocol and hostname tuple must be unique for every listener",
				)
			}
			listenerSets.Insert(portProtocolHostname)
		}
	}
}

func (t *Translator) validateConflictedLayer7Listeners(gateways []*GatewayContext) {
	// Iterate through all layer-7 (HTTP, HTTPS, TLS) listeners and collect info about protocols
	// and hostnames per port.
	for _, gateway := range gateways {
		portListenerInfo := map[gwapiv1.PortNumber]*portListeners{}
		for _, listener := range gateway.listeners {
			if listener.Protocol == gwapiv1.UDPProtocolType || listener.Protocol == gwapiv1.TCPProtocolType {
				continue
			}
			if portListenerInfo[listener.Port] == nil {
				portListenerInfo[listener.Port] = &portListeners{
					protocols: sets.Set[string]{},
					hostnames: map[string]int{},
				}
			}

			portListenerInfo[listener.Port].listeners = append(portListenerInfo[listener.Port].listeners, listener)

			var protocol string
			switch listener.Protocol {
			// HTTPS and TLS can co-exist on the same port
			case gwapiv1.HTTPSProtocolType, gwapiv1.TLSProtocolType:
				protocol = "https/tls"
			default:
				protocol = string(listener.Protocol)
			}
			portListenerInfo[listener.Port].protocols.Insert(protocol)

			var hostname string
			if listener.Hostname != nil {
				hostname = string(*listener.Hostname)
			}

			portListenerInfo[listener.Port].hostnames[hostname]++
		}

		// Set Conflicted conditions for any listeners with conflicting specs.
		for _, info := range portListenerInfo {
			for _, listener := range info.listeners {
				if len(info.protocols) > 1 {
					status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
						listener.listenerStatusIdx,
						gwapiv1.ListenerConditionConflicted,
						metav1.ConditionTrue,
						gwapiv1.ListenerReasonProtocolConflict,
						"All listeners for a given port must use a compatible protocol",
					)
				}

				var hostname string
				if listener.Hostname != nil {
					hostname = string(*listener.Hostname)
				}

				if info.hostnames[hostname] > 1 {
					status.SetGatewayListenerStatusCondition(listener.gateway.Gateway,
						listener.listenerStatusIdx,
						gwapiv1.ListenerConditionConflicted,
						metav1.ConditionTrue,
						gwapiv1.ListenerReasonHostnameConflict,
						"All listeners for a given port must use a unique hostname",
					)
				}
			}
		}
	}
}

func (t *Translator) validateConflictedLayer4Listeners(gateways []*GatewayContext, protocols ...gwapiv1.ProtocolType) {
	// Iterate through all layer-4(TCP UDP) listeners and check if there are more than one listener on the same port
	for _, gateway := range gateways {
		portListenerInfo := map[gwapiv1.PortNumber]*portListeners{}
		for _, listener := range gateway.listeners {
			for _, protocol := range protocols {
				if listener.Protocol == protocol {
					if portListenerInfo[listener.Port] == nil {
						portListenerInfo[listener.Port] = &portListeners{}
					}
					portListenerInfo[listener.Port].listeners = append(portListenerInfo[listener.Port].listeners, listener)
				}
			}
		}

		// Leave the first one and set Conflicted conditions for all other listeners with conflicting specs.
		for _, info := range portListenerInfo {
			if len(info.listeners) > 1 {
				for i := 1; i < len(info.listeners); i++ {
					status.SetGatewayListenerStatusCondition(info.listeners[i].gateway.Gateway,
						info.listeners[i].listenerStatusIdx,
						gwapiv1.ListenerConditionConflicted,
						metav1.ConditionTrue,
						gwapiv1.ListenerReasonProtocolConflict,
						fmt.Sprintf("Only one %s listener is allowed in a given port", strings.Join(protocolSliceToStringSlice(protocols), "/")),
					)
				}
			}
		}
	}
}

func (t *Translator) validateCrossNamespaceRef(from crossNamespaceFrom, to crossNamespaceTo, referenceGrants []*gwapiv1b1.ReferenceGrant) bool {
	for _, referenceGrant := range referenceGrants {
		// The ReferenceGrant must be defined in the namespace of
		// the "to" (the referent).
		if referenceGrant.Namespace != to.namespace {
			continue
		}

		// Check if the ReferenceGrant has a matching "from".
		var fromAllowed bool
		for _, refGrantFrom := range referenceGrant.Spec.From {
			if string(refGrantFrom.Namespace) == from.namespace && string(refGrantFrom.Group) == from.group && string(refGrantFrom.Kind) == from.kind {
				fromAllowed = true
				break
			}
		}
		if !fromAllowed {
			continue
		}

		// Check if the ReferenceGrant has a matching "to".
		var toAllowed bool
		for _, refGrantTo := range referenceGrant.Spec.To {
			if string(refGrantTo.Group) == to.group && string(refGrantTo.Kind) == to.kind && (refGrantTo.Name == nil || *refGrantTo.Name == "" || string(*refGrantTo.Name) == to.name) {
				toAllowed = true
				break
			}
		}
		if !toAllowed {
			continue
		}

		// If we got here, both the "from" and the "to" were allowed by this
		// reference grant.
		return true
	}

	// If we got here, no reference policy or reference grant allowed both the "from" and "to".
	return false
}

// Checks if a hostname is valid according to RFC 1123 and gateway API's requirement that it not be an IP address
func (t *Translator) validateHostname(hostname string) error {
	if errs := validation.IsDNS1123Subdomain(hostname); errs != nil {
		return fmt.Errorf("hostname %q is invalid: %v", hostname, errs)
	}

	// IP addresses are not allowed so parsing the hostname as an address needs to fail
	if _, err := netip.ParseAddr(hostname); err == nil {
		return fmt.Errorf("hostname: %q cannot be an ip address", hostname)
	}

	labelIdx := 0
	for i := range hostname {
		if hostname[i] == '.' {

			if i-labelIdx > 63 {
				return fmt.Errorf("label: %q in hostname %q cannot exceed 63 characters", hostname[labelIdx:i], hostname)
			}
			labelIdx = i + 1
		}
	}
	// Check the last label
	if len(hostname)-labelIdx > 63 {
		return fmt.Errorf("label: %q in hostname %q cannot exceed 63 characters", hostname[labelIdx:], hostname)
	}

	return nil
}

// validateSecretRef checks three things:
//  1. Does the secret reference have a valid Group and kind
//  2. If the secret reference is a cross-namespace reference,
//     is it permitted by any ReferenceGrant
//  3. Does the secret exist
//
// nolint:unparam
func (t *Translator) validateSecretRef(
	allowCrossNamespace bool,
	from crossNamespaceFrom,
	secretObjRef gwapiv1.SecretObjectReference,
	resources *resource.Resources,
) (*corev1.Secret, error) {
	if err := t.validateSecretObjectRef(allowCrossNamespace, from, secretObjRef, resources); err != nil {
		return nil, err
	}

	secretNamespace := from.namespace
	if secretObjRef.Namespace != nil {
		secretNamespace = string(*secretObjRef.Namespace)
	}
	secret := resources.GetSecret(secretNamespace, string(secretObjRef.Name))

	if secret == nil {
		return nil, fmt.Errorf(
			"secret %s/%s does not exist", secretNamespace, secretObjRef.Name)
	}

	return secret, nil
}

func (t *Translator) validateConfigMapRef(
	allowCrossNamespace bool,
	from crossNamespaceFrom,
	secretObjRef gwapiv1.SecretObjectReference,
	resources *resource.Resources,
) (*corev1.ConfigMap, error) {
	if err := t.validateSecretObjectRef(allowCrossNamespace, from, secretObjRef, resources); err != nil {
		return nil, err
	}

	configMapNamespace := from.namespace
	if secretObjRef.Namespace != nil {
		configMapNamespace = string(*secretObjRef.Namespace)
	}
	configMap := resources.GetConfigMap(configMapNamespace, string(secretObjRef.Name))

	if configMap == nil {
		return nil, fmt.Errorf(
			"configmap %s/%s does not exist", configMapNamespace, secretObjRef.Name)
	}

	return configMap, nil
}

func (t *Translator) validateSecretObjectRef(
	allowCrossNamespace bool,
	from crossNamespaceFrom,
	secretRef gwapiv1.SecretObjectReference,
	resources *resource.Resources,
) error {
	var kind string
	if secretRef.Group != nil && string(*secretRef.Group) != "" {
		return errors.New("secret ref group must be unspecified/empty")
	}

	if secretRef.Kind == nil { // nolint
		kind = resource.KindSecret
	} else if string(*secretRef.Kind) == resource.KindSecret {
		kind = resource.KindSecret
	} else if string(*secretRef.Kind) == resource.KindConfigMap {
		kind = resource.KindConfigMap
	} else {
		return fmt.Errorf("secret ref kind must be %s", resource.KindSecret)
	}

	if secretRef.Namespace != nil &&
		string(*secretRef.Namespace) != "" &&
		string(*secretRef.Namespace) != from.namespace {
		if !allowCrossNamespace {
			return fmt.Errorf(
				"secret ref namespace must be unspecified/empty or %s",
				from.namespace)
		}

		if !t.validateCrossNamespaceRef(
			from,
			crossNamespaceTo{
				group:     "",
				kind:      kind,
				namespace: string(*secretRef.Namespace),
				name:      string(secretRef.Name),
			},
			resources.ReferenceGrants,
		) {
			return fmt.Errorf(
				"certificate ref to secret %s/%s not permitted by any ReferenceGrant",
				*secretRef.Namespace, secretRef.Name)
		}

	}

	return nil
}

// TODO: zhaohuabing combine this function with the one in the route translator
// validateExtServiceBackendReference validates the backend reference for an
// external service referenced by an EG policy.
// This can also be used for the other external services deployed in the cluster,
// such as the external processing filter, gRPC Access Log Service, etc.
// It checks:
//  1. The group is nil or empty, indicating the core API group, or gateway.envoyproxy.io
//  2. The kind is Service or Backend.
//  3. The port is specified for Services.
//  4. The Service or Backend exists and the specified port is found.
//  5. The cross-namespace reference is permitted by the ReferenceGrants if the
//     namespace is different from the policy's namespace.
func (t *Translator) validateExtServiceBackendReference(
	backendRef *gwapiv1.BackendObjectReference,
	ownerNamespace string,
	policyKind string,
	resources *resource.Resources,
) error {
	// These are sanity checks, they should never happen because the API server
	// should have caught them
	if backendRef.Group != nil && *backendRef.Group != "" && *backendRef.Group != GroupMultiClusterService && *backendRef.Group != egv1a1.GroupName {
		return fmt.Errorf("group is invalid, only the core API group (specified by omitting the group field or setting it to an empty string), the %s API group, and the %s API group are supported", GroupMultiClusterService, egv1a1.GroupName)
	}
	if backendRef.Kind != nil && *backendRef.Kind != resource.KindService && *backendRef.Kind != resource.KindServiceImport && *backendRef.Kind != egv1a1.KindBackend {
		return errors.New("kind is invalid, only Service (specified by omitting " +
			"the kind field or setting it to 'Service'), ServiceImport, and Backend are supported")
	}
	if backendRef.Port == nil && (backendRef.Kind == nil || *backendRef.Kind != egv1a1.KindBackend) {
		return errors.New("a valid port number corresponding to a port on the Service must be specified")
	}

	backendRefKind := KindDerefOr(backendRef.Kind, resource.KindService)
	switch backendRefKind {
	case resource.KindService:
		// check if the service is valid
		serviceNamespace := NamespaceDerefOr(backendRef.Namespace, ownerNamespace)
		service := resources.GetService(serviceNamespace, string(backendRef.Name))
		if service == nil {
			return fmt.Errorf("service %s/%s not found", serviceNamespace, backendRef.Name)
		}
		var portFound bool
		for _, port := range service.Spec.Ports {
			portProtocol := port.Protocol
			if port.Protocol == "" { // Default protocol is TCP
				portProtocol = corev1.ProtocolTCP
			}
			// currently only HTTP and GRPC are supported, both of which are TCP
			if port.Port == int32(*backendRef.Port) && portProtocol == corev1.ProtocolTCP {
				portFound = true
				break
			}
		}

		if !portFound {
			return fmt.Errorf(
				"TCP Port %d not found on service %s/%s",
				*backendRef.Port, serviceNamespace, string(backendRef.Name),
			)
		}
	case resource.KindServiceImport:
		// check if the service import is valid
		serviceImportNamespace := NamespaceDerefOr(backendRef.Namespace, ownerNamespace)
		serviceImport := resources.GetServiceImport(serviceImportNamespace, string(backendRef.Name))
		if serviceImport == nil {
			return fmt.Errorf("serviceimport %s/%s not found", serviceImportNamespace, backendRef.Name)
		}
		var portFound bool
		for _, port := range serviceImport.Spec.Ports {
			portProtocol := port.Protocol
			if port.Protocol == "" { // Default protocol is TCP
				portProtocol = corev1.ProtocolTCP
			}
			// currently only HTTP and GRPC are supported, both of which are TCP
			if port.Port == int32(*backendRef.Port) && portProtocol == corev1.ProtocolTCP {
				portFound = true
				break
			}
		}

		if !portFound {
			return fmt.Errorf(
				"TCP Port %d not found on service %s/%s",
				*backendRef.Port, serviceImportNamespace, string(backendRef.Name),
			)
		}
	case egv1a1.KindBackend:
		backendNamespace := NamespaceDerefOr(backendRef.Namespace, ownerNamespace)
		backend := resources.GetBackend(backendNamespace, string(backendRef.Name))
		if backend == nil {
			return fmt.Errorf("backend %s/%s not found", backendNamespace, backendRef.Name)
		}
		// Dynamic resolver backend is not supported for EG policies
		if backend.Spec.Type != nil && *backend.Spec.Type == egv1a1.BackendTypeDynamicResolver {
			return fmt.Errorf("dynamic resolver backend %s/%s is not supported", backendNamespace, backendRef.Name)
		}
	}

	// check if the cross-namespace reference is permitted
	if backendRef.Namespace != nil && string(*backendRef.Namespace) != "" &&
		string(*backendRef.Namespace) != ownerNamespace {
		if !t.validateCrossNamespaceRef(
			crossNamespaceFrom{
				group:     egv1a1.GroupName,
				kind:      policyKind,
				namespace: ownerNamespace,
			},
			crossNamespaceTo{
				group:     GroupDerefOr(backendRef.Group, ""),
				kind:      KindDerefOr(backendRef.Kind, backendRefKind),
				namespace: string(*backendRef.Namespace),
				name:      string(backendRef.Name),
			},
			resources.ReferenceGrants,
		) {
			return fmt.Errorf(
				"backend ref to %s %s/%s not permitted by any ReferenceGrant",
				backendRefKind, *backendRef.Namespace, backendRef.Name)
		}
	}
	return nil
}

// validateGatewayListenerSectionName check:
// if the section name exists in the target Gateway listeners.
func validateGatewayListenerSectionName(
	sectionName gwapiv1.SectionName,
	targetKey types.NamespacedName,
	listeners []*ListenerContext,
) *status.PolicyResolveError {
	found := false
	for _, l := range listeners {
		if l.Name == sectionName {
			found = true
			break
		}
	}
	if !found {
		message := fmt.Sprintf("No section name %s found for Gateway %s",
			string(sectionName), targetKey.String())

		return &status.PolicyResolveError{
			Reason:  gwapiv1a2.PolicyReasonTargetNotFound,
			Message: message,
		}
	}
	return nil
}
