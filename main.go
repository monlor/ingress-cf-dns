package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ingress-cf-dns/config"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zones"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	logrus "github.com/sirupsen/logrus"
)

type Controller struct {
	kubeClient *kubernetes.Clientset
	cfAPI      *cloudflare.Client
	config     *config.Config
	stopCh     chan struct{}
}

func main() {
	// 初始化logrus
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	logrus.SetLevel(logrus.InfoLevel)

	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		logrus.Fatalf("Error loading configuration: %s", err.Error())
	}

	// Create kubernetes client
	var kubeClient *kubernetes.Clientset

	// Try to use kubeconfig from user's home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logrus.Fatalf("Error getting user home directory: %s", err.Error())
	}
	kubeconfigPath := filepath.Join(homeDir, ".kube", "config")
	if _, err := os.Stat(kubeconfigPath); err == nil {
		// Use kubeconfig file from home directory
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			logrus.Fatalf("Error building kubeconfig: %s", err.Error())
		}
		kubeClient, err = kubernetes.NewForConfig(config)
	} else {
		// Use in-cluster config
		config, err := rest.InClusterConfig()
		if err != nil {
			logrus.Fatalf("Error getting in-cluster config: %s", err.Error())
		}
		kubeClient, err = kubernetes.NewForConfig(config)

		if err != nil {
			logrus.Fatalf("Error building kubernetes client: %s", err.Error())
		}
	}

	// Create Cloudflare client
	cfAPI := cloudflare.NewClient(
		option.WithAPIToken(cfg.CloudflareToken),
	)

	controller := &Controller{
		kubeClient: kubeClient,
		cfAPI:      cfAPI,
		config:     cfg,
	}

	// Start the controller
	controller.Run()
}

func (c *Controller) Run() {
	logrus.Infof("Starting controller with sync interval: %v", c.config.SyncInterval)

	c.stopCh = make(chan struct{})

	// Run initial sync
	c.syncAll()

	// Start watching Ingress resources
	go c.watchIngresses()

	// Start periodic sync for catching any missed updates
	ticker := time.NewTicker(c.config.SyncInterval)
	for {
		select {
		case <-ticker.C:
			c.syncAll()
		case <-c.stopCh:
			ticker.Stop()
			return
		}
	}
}

func (c *Controller) syncAll() {
	logrus.Debug("Starting sync...")

	// Build label selector to only get ingresses with DNS management enabled
	labelSelector := fmt.Sprintf("%s=%s", c.config.IngressLabelKey, c.config.IngressLabelValue)
	
	// Get ingresses with DNS management enabled from all namespaces
	ingresses, err := c.kubeClient.NetworkingV1().Ingresses("").List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		logrus.Errorf("Error listing ingresses: %s", err.Error())
		return
	}

	// Process each ingress
	for _, ing := range ingresses.Items {
		// Skip if namespace is not allowed
		if !c.config.IsNamespaceAllowed(ing.Namespace) {
			logrus.Debugf("Skipping ingress %s/%s: namespace not allowed by pattern %s",
				ing.Namespace, ing.Name, c.config.NamespaceRegex)
			continue
		}
		
		c.processIngressWithRetry(&ing)
	}

	logrus.Debug("Sync completed")
}

func (c *Controller) watchIngresses() {
	// Build label selector to only watch ingresses with DNS management enabled
	labelSelector := fmt.Sprintf("%s=%s", c.config.IngressLabelKey, c.config.IngressLabelValue)
	
	for {
		watcher, err := c.kubeClient.NetworkingV1().Ingresses("").Watch(context.Background(), metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			logrus.Errorf("Error watching ingresses: %s", err.Error())
			time.Sleep(5 * time.Second)
			continue
		}

		for event := range watcher.ResultChan() {
			switch event.Type {
			case watch.Added, watch.Modified:
				ing := event.Object.(*networkingv1.Ingress)
				// Process ingress asynchronously to not block the watch
				logrus.Infof("Processing ingress %s/%s", ing.Namespace, ing.Name)
				go c.processIngressWithRetry(ing)
			case watch.Deleted:
				ing := event.Object.(*networkingv1.Ingress)
				// Process ingress deletion asynchronously to not block the watch
				logrus.Infof("Processing ingress deletion %s/%s", ing.Namespace, ing.Name)
				go c.processIngressDeletion(ing)
			}
		}

		select {
		case <-c.stopCh:
			return
		default:
			// Reconnect the watcher if it was closed
			time.Sleep(1 * time.Second)
		}
	}
}

func (c *Controller) processIngressWithRetry(ing *networkingv1.Ingress) {
	// Skip if namespace is not allowed
	if !c.config.IsNamespaceAllowed(ing.Namespace) {
		logrus.Warnf("Skipping ingress %s/%s: namespace not allowed by pattern %s",
			ing.Namespace, ing.Name, c.config.NamespaceRegex)
		return
	}

	// Retry logic for handling pod scheduling delays
	backoff := wait.Backoff{
		Steps:    15,              // Maximum number of retries
		Duration: 2 * time.Second, // Initial retry interval
		Factor:   1.5,             // Multiply duration by this factor each iteration
		Jitter:   0.1,             // Add this much jitter to duration
	}

	var lastErr error
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		err := c.processIngressInternal(ing)
		if err != nil {
			lastErr = err
			if strings.Contains(err.Error(), "no pods found") ||
				strings.Contains(err.Error(), "has no node assigned") {
				// Retry for pod scheduling related errors
				logrus.Debugf("Retrying ingress %s/%s processing due to: %s",
					ing.Namespace, ing.Name, err.Error())
				return false, nil
			}
			// Don't retry for other errors
			return false, err
		}
		return true, nil
	})

	if err != nil {
		if err == wait.ErrWaitTimeout {
			logrus.Errorf("Timeout waiting for ingress %s/%s pods to be ready: %s",
				ing.Namespace, ing.Name, lastErr.Error())
		} else {
			logrus.Errorf("Error processing ingress %s/%s: %s",
				ing.Namespace, ing.Name, err.Error())
		}
	}
}

func (c *Controller) processIngressDeletion(ing *networkingv1.Ingress) {
	// Skip if namespace is not allowed
	if !c.config.IsNamespaceAllowed(ing.Namespace) {
		logrus.Debugf("Skipping ingress deletion %s/%s: namespace not allowed by pattern %s",
			ing.Namespace, ing.Name, c.config.NamespaceRegex)
		return
	}

	// Collect all hosts from the deleted ingress
	var hostsToDelete []string
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}

		// Skip if domain is not allowed
		if !c.config.IsDomainAllowed(rule.Host) {
			logrus.Debugf("Skipping host %s in deleted ingress %s/%s: domain not allowed by pattern %s",
				rule.Host, ing.Namespace, ing.Name, c.config.DomainRegex)
			continue
		}

		hostsToDelete = append(hostsToDelete, rule.Host)
	}

	// Delete DNS records for all hosts
	for _, host := range hostsToDelete {
		if err := c.deleteDNSRecord(host); err != nil {
			logrus.Errorf("Error deleting DNS record for %s: %s", host, err.Error())
		}
	}

	logrus.Infof("Processed deletion of ingress %s/%s, deleted DNS records for %d hosts",
		ing.Namespace, ing.Name, len(hostsToDelete))
}

func (c *Controller) processIngressInternal(ing *networkingv1.Ingress) error {
	// Map to track host to backend mapping
	hostBackends := make(map[string]string)
	backendHosts := make(map[string][]string)

	// First pass: collect and validate host-backend mappings
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" || rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
			continue
		}

		// Skip if domain is not allowed
		if !c.config.IsDomainAllowed(rule.Host) {
			logrus.Debugf("Skipping host %s in ingress %s/%s: domain not allowed by pattern %s",
				rule.Host, ing.Namespace, ing.Name, c.config.DomainRegex)
			continue
		}

		// Only consider the first path's backend
		path := rule.HTTP.Paths[0]
		if path.Backend.Service == nil {
			continue
		}

		backendName := path.Backend.Service.Name

		// Check if this host already has a different backend
		if existingBackend, exists := hostBackends[rule.Host]; exists {
			if existingBackend != backendName {
				logrus.Debugf("Host %s already mapped to backend %s, skipping new backend %s",
					rule.Host, existingBackend, backendName)
			}
			continue
		}

		// Store the valid mapping
		hostBackends[rule.Host] = backendName
		backendHosts[backendName] = append(backendHosts[backendName], rule.Host)
	}

	// Process valid host-backend pairs
	for backendName, hosts := range backendHosts {
		// Get service
		svc, err := c.kubeClient.CoreV1().Services(ing.Namespace).Get(
			context.Background(),
			backendName,
			metav1.GetOptions{},
		)
		if err != nil {
			return fmt.Errorf("error getting service %s: %w", backendName, err)
		}

		// Get pods for this service
		pods, err := c.getPodsForService(svc)
		if err != nil {
			return fmt.Errorf("error getting pods for service %s: %w", svc.Name, err)
		}

		// Return error if no pods found to trigger retry
		if len(pods.Items) == 0 {
			return fmt.Errorf("no pods found for service %s", svc.Name)
		}

		// Get the first pod's node
		pod := pods.Items[0]
		if pod.Spec.NodeName == "" {
			return fmt.Errorf("pod %s has no node assigned", pod.Name)
		}

		// Get node
		node, err := c.kubeClient.CoreV1().Nodes().Get(
			context.Background(),
			pod.Spec.NodeName,
			metav1.GetOptions{},
		)
		if err != nil {
			return fmt.Errorf("error getting node %s: %w", pod.Spec.NodeName, err)
		}

		// Get public IP from node annotation
		publicIP, ok := node.Annotations[c.config.NodePublicIPAnnotation]
		if !ok {
			return fmt.Errorf("node %s does not have public IP annotation", node.Name)
		}

		// Update DNS records for all hosts of this backend
		for _, host := range hosts {
			if err := c.updateDNSRecord(host, publicIP); err != nil {
				return fmt.Errorf("error updating DNS record for %s: %w", host, err)
			}
		}
	}

	return nil
}

// getPodsForService returns pods that match the service's selector
func (c *Controller) getPodsForService(svc *corev1.Service) (*corev1.PodList, error) {
	// Convert service selector to label selector string
	if svc.Spec.Selector == nil {
		return nil, fmt.Errorf("service %s has no selector", svc.Name)
	}

	// Build label selector
	var selectors []string
	for key, value := range svc.Spec.Selector {
		selectors = append(selectors, fmt.Sprintf("%s=%s", key, value))
	}
	labelSelector := strings.Join(selectors, ",")

	// Get pods matching the service selector
	return c.kubeClient.CoreV1().Pods(svc.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
}

// getZoneIDByDomain gets the Cloudflare Zone ID for a given domain
func (c *Controller) getZoneIDByDomain(domain string) (string, error) {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid domain format: %s", domain)
	}

	for i := len(parts) - 2; i >= 0; i-- {
		searchDomain := strings.Join(parts[i:], ".")

		// Use ZoneListParams to search for zones
		listParams := zones.ZoneListParams{
			Name:      cloudflare.F(searchDomain),
			Status:    cloudflare.F(zones.ZoneListParamsStatusActive),
			PerPage:   cloudflare.F(50.0),
			Direction: cloudflare.F(zones.ZoneListParamsDirectionAsc),
		}

		result, err := c.cfAPI.Zones.List(context.Background(), listParams)
		if err != nil {
			logrus.Debugf("Error listing zones: %s", err.Error())
			continue
		}

		for _, zone := range result.Result {
			if strings.HasSuffix(domain, zone.Name) {
				return zone.ID, nil
			}
		}
	}

	return "", fmt.Errorf("no matching zone found for domain: %s", domain)
}

func (c *Controller) updateDNSRecord(host, publicIP string) error {
	// Get Zone ID for the host
	zoneID, err := c.getZoneIDByDomain(host)
	if err != nil {
		return fmt.Errorf("error getting zone ID: %w", err)
	}

	ctx := context.Background()

	// Check if record exists
	listParams := dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(host),
		}),
	}

	records, err := c.cfAPI.DNS.Records.List(ctx, listParams)
	if err != nil {
		return fmt.Errorf("error checking DNS record: %w", err)
	}

	if len(records.Result) > 0 {
		// Check if update is needed
		record := records.Result[0]

		// Check if record is managed by another controller
		if strings.HasPrefix(strings.ToLower(record.Comment), "managed by") && record.Comment != config.DNSRecordComment {
			logrus.Debugf("DNS record for %s is managed by another controller: %s", host, record.Comment)
			return nil
		}

		if record.Content == publicIP {
			logrus.Debugf("DNS record for %s already points to %s in zone ID %s, skipping update",
				host, publicIP, zoneID)
			return nil
		}

		// Update existing record
		updateParams := dns.RecordUpdateParams{
			ZoneID: cloudflare.F(zoneID),
			Record: dns.ARecordParam{
				Name:    cloudflare.F(host),
				Type:    cloudflare.F(dns.ARecordTypeA),
				Content: cloudflare.F(publicIP),
				Proxied: cloudflare.F(record.Proxied),
				TTL:     cloudflare.F(dns.TTL1),
				Comment: cloudflare.F(config.DNSRecordComment),
			},
		}
		_, err = c.cfAPI.DNS.Records.Update(ctx, record.ID, updateParams)
		if err != nil {
			return fmt.Errorf("error updating DNS record: %w", err)
		}

		logrus.Infof("Successfully updated DNS record for %s from %s to %s in zone ID %s (proxied: %v)",
			host, record.Content, publicIP, zoneID, record.Proxied)
	} else {
		// Create new record
		newParams := dns.RecordNewParams{
			ZoneID: cloudflare.F(zoneID),
			Record: dns.ARecordParam{
				Name:    cloudflare.F(host),
				Type:    cloudflare.F(dns.ARecordTypeA),
				Content: cloudflare.F(publicIP),
				Proxied: cloudflare.F(c.config.DNSProxied),
				TTL:     cloudflare.F(dns.TTL1),
				Comment: cloudflare.F(config.DNSRecordComment),
			},
		}
		_, err = c.cfAPI.DNS.Records.New(ctx, newParams)
		if err != nil {
			return fmt.Errorf("error creating DNS record: %w", err)
		}

		logrus.Infof("Successfully created DNS record for %s to %s in zone ID %s (proxied: %v)",
			host, publicIP, zoneID, c.config.DNSProxied)
	}

	return nil
}

func (c *Controller) deleteDNSRecord(host string) error {
	// Get Zone ID for the host
	zoneID, err := c.getZoneIDByDomain(host)
	if err != nil {
		return fmt.Errorf("error getting zone ID: %w", err)
	}

	ctx := context.Background()

	// Check if record exists
	listParams := dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(host),
		}),
	}

	records, err := c.cfAPI.DNS.Records.List(ctx, listParams)
	if err != nil {
		return fmt.Errorf("error checking DNS record: %w", err)
	}

	if len(records.Result) > 0 {
		record := records.Result[0]
		
		// Check if record is managed by this controller
		if record.Comment != config.DNSRecordComment {
			logrus.Debugf("DNS record for %s is not managed by this controller: %s", host, record.Comment)
			return nil
		}
		
		// Delete the record using RecordDeleteParams
		deleteParams := dns.RecordDeleteParams{
			ZoneID: cloudflare.F(zoneID),
		}
		_, err = c.cfAPI.DNS.Records.Delete(ctx, record.ID, deleteParams)
		if err != nil {
			return fmt.Errorf("error deleting DNS record: %w", err)
		}

		logrus.Infof("Successfully deleted DNS record for %s in zone ID %s", host, zoneID)
	} else {
		logrus.Debugf("DNS record for %s does not exist in zone ID %s", host, zoneID)
	}

	return nil
}
