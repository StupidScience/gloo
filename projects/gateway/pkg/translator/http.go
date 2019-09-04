package translator

import (
	"context"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/solo-io/gloo/projects/gateway/pkg/api/v1"
	v2 "github.com/solo-io/gloo/projects/gateway/pkg/api/v2"
	gloov1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	glooutils "github.com/solo-io/gloo/projects/gloo/pkg/utils"
	"github.com/solo-io/go-utils/contextutils"
	"github.com/solo-io/solo-kit/pkg/api/v1/reporter"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"
)

type HttpTranslator struct{}

func (t *HttpTranslator) GenerateListeners(ctx context.Context, snap *v2.ApiSnapshot, filteredGateways []*v2.Gateway, resourceErrs reporter.ResourceErrors) []*gloov1.Listener {
	if len(snap.VirtualServices) == 0 {
		contextutils.LoggerFrom(ctx).Debugf("%v had no virtual services", snap.Hash())
		return nil
	}
	var result []*gloov1.Listener
	for _, gateway := range filteredGateways {
		if gateway.GetHttpGateway() == nil {
			continue
		}

		virtualServices := getVirtualServiceForGateway(gateway, snap.VirtualServices, resourceErrs)
		filtered := filterVirtualServiceForGateway(gateway, virtualServices)
		mergedVirtualServices := validateAndMergeVirtualServices(gateway, filtered, resourceErrs)
		listener := desiredListenerForHttp(gateway, mergedVirtualServices)
		result = append(result, listener)
	}
	return result
}

func domainsToKey(domains []string) string {
	// copy before mutating for good measure
	domains = append([]string{}, domains...)
	// sort, and join all domains with an out of band character, like ','
	sort.Strings(domains)
	return strings.Join(domains, ",")
}

func validateAndMergeVirtualServices(gateway *v2.Gateway, virtualServices v1.VirtualServiceList, resourceErrs reporter.ResourceErrors) v1.VirtualServiceList {
	ns := gateway.Metadata.GetNamespace()
	domainKeysSets := map[string]v1.VirtualServiceList{}
	for _, vs := range virtualServices {
		if vs.VirtualHost == nil {
			continue
		}
		domainsKey := domainsToKey(vs.VirtualHost.Domains)
		domainKeysSets[domainsKey] = append(domainKeysSets[domainsKey], vs)
	}

	domainSet := map[string][]string{}
	// make sure each domain is only in one domain set
	for k, vslist := range domainKeysSets {
		// take the first one as they are all the same
		domains := vslist[0].VirtualHost.Domains
		for _, d := range domains {
			domainSet[d] = append(domainSet[d], k)
		}
	}

	// report errors
	for domain, domainSetKeys := range domainSet {
		if len(domainSetKeys) > 1 {
			resourceErrs.AddError(gateway, fmt.Errorf("domain %s is present in more than one vservice set in this gateway", domain))
		}
	}
	// return merged list
	var mergedVirtualServices v1.VirtualServiceList
	for k, vslist := range domainKeysSets {
		if len(vslist) == 1 {
			// only one vservice, do nothing.
			mergedVirtualServices = append(mergedVirtualServices, vslist[0])
			continue
		}

		// take the first one as they are all the same
		var routes []*v1.Route
		var sslConfig *gloov1.SslConfig
		var vhostPlugins *gloov1.VirtualHostPlugins
		for _, vs := range vslist {
			routes = append(routes, vs.VirtualHost.Routes...)
			if sslConfig == nil {
				sslConfig = vs.SslConfig
			} else if !vs.SslConfig.Equal(sslConfig) {
				resourceErrs.AddError(gateway, fmt.Errorf("more than one distinct ssl config is present in virtual service of these domains: %s", k))
			}

			havePlugins := vs.VirtualHost != nil &&
				vs.VirtualHost.VirtualHostPlugins != nil

			if vhostPlugins == nil {
				if havePlugins {
					vhostPlugins = vs.VirtualHost.VirtualHostPlugins
				}
			} else if havePlugins {
				resourceErrs.AddError(gateway, fmt.Errorf("more than one vhost plugin is present in virtual service of these domains: %s", k))
			}
		}

		glooutils.SortGatewayRoutesByPath(routes)

		ref := core.Metadata{
			// name shouldnt matter as it this object is ephemeral.
			Name:      getMergedName(k),
			Namespace: ns,
		}
		mergedVs := &v1.VirtualService{
			VirtualHost: &v1.VirtualHost{
				Domains:            vslist[0].VirtualHost.Domains,
				Routes:             routes,
				VirtualHostPlugins: vhostPlugins,
			},
			SslConfig: sslConfig,
			Metadata:  ref,
		}
		mergedVirtualServices = append(mergedVirtualServices, mergedVs)
	}

	return mergedVirtualServices
}

func getMergedName(k string) string {
	if k == "" {
		return "catchall"
	}
	return "merged-" + k
}

func getVirtualServiceForGateway(gateway *v2.Gateway, virtualServices v1.VirtualServiceList, resourceErrs reporter.ResourceErrors) v1.VirtualServiceList {
	httpGateway := gateway.GetHttpGateway()
	if httpGateway == nil {
		return nil
	}
	virtualServiceRefs := httpGateway.VirtualServices
	// add all virtual services if empty
	if len(virtualServiceRefs) == 0 {
		for _, virtualService := range virtualServices {
			virtualServiceRefs = append(virtualServiceRefs, core.ResourceRef{
				Name:      virtualService.GetMetadata().Name,
				Namespace: virtualService.GetMetadata().Namespace,
			})
		}
	}

	var virtualServicesForGateway v1.VirtualServiceList
	for _, ref := range virtualServiceRefs {
		virtualService, err := virtualServices.Find(ref.Strings())
		if err != nil {
			resourceErrs.AddError(gateway, err)
			continue
		}
		virtualServicesForGateway = append(virtualServicesForGateway, virtualService)
	}

	return virtualServicesForGateway
}

func filterVirtualServiceForGateway(gateway *v2.Gateway, virtualServices v1.VirtualServiceList) v1.VirtualServiceList {
	var virtualServicesForGateway v1.VirtualServiceList
	for _, virtualService := range virtualServices {
		if gateway.Ssl == hasSsl(virtualService) {
			virtualServicesForGateway = append(virtualServicesForGateway, virtualService)
		}
	}
	return virtualServicesForGateway
}

func hasSsl(vs *v1.VirtualService) bool {
	return vs.SslConfig != nil
}

func desiredListenerForHttp(gateway *v2.Gateway, virtualServicesForGateway v1.VirtualServiceList) *gloov1.Listener {
	var (
		virtualHosts []*gloov1.VirtualHost
		sslConfigs   []*gloov1.SslConfig
	)

	for _, virtualService := range virtualServicesForGateway {
		ref := virtualService.Metadata.Ref()
		if virtualService.VirtualHost == nil {
			virtualService.VirtualHost = &v1.VirtualHost{}
		}
		virtualHosts = append(virtualHosts, convertVirtualHost(ref, virtualService.VirtualHost))
		if virtualService.SslConfig != nil {
			sslConfigs = append(sslConfigs, virtualService.SslConfig)
		}
	}

	var httpPlugins *gloov1.HttpListenerPlugins
	if httpGateway := gateway.GetHttpGateway(); httpGateway != nil {
		httpPlugins = httpGateway.Plugins
	}
	listener := standardListener(gateway)
	listener.ListenerType = &gloov1.Listener_HttpListener{
		HttpListener: &gloov1.HttpListener{
			VirtualHosts:    virtualHosts,
			ListenerPlugins: httpPlugins,
		},
	}
	listener.SslConfigurations = sslConfigs
	return listener
}

func convertVirtualHost(vs core.ResourceRef, ours *v1.VirtualHost) *gloov1.VirtualHost {
	vh := &gloov1.VirtualHost{
		Name:               fmt.Sprintf("%v.%v", vs.Namespace, vs.Name),
		Domains:            ours.Domains,
		Routes:             convertRoutes(ours.Routes),
		VirtualHostPlugins: ours.VirtualHostPlugins,
	}

	return vh
}

func convertRoutes(ours []*v1.Route) []*gloov1.Route {
	var routes []*gloov1.Route
	for _, r := range ours {
		routes = append(routes, convertRoute(r))
	}
	return routes
}

func convertRoute(ours *v1.Route) *gloov1.Route {
	route := &gloov1.Route{
		Matcher:      ours.Matcher,
		RoutePlugins: ours.RoutePlugins,
	}
	switch action := ours.Action.(type) {
	case *v1.Route_RedirectAction:
		route.Action = &gloov1.Route_RedirectAction{
			RedirectAction: action.RedirectAction,
		}
	case *v1.Route_DirectResponseAction:
		route.Action = &gloov1.Route_DirectResponseAction{
			DirectResponseAction: action.DirectResponseAction,
		}
	case *v1.Route_RouteAction:
		route.Action = &gloov1.Route_RouteAction{
			RouteAction: action.RouteAction,
		}
	case *v1.Route_DelegateAction:
		panic("not implemented")
	}
	return route
}