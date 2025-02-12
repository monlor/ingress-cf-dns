package main

import (
    "context"
    "fmt"
    "log"
    "strings"
    "time"
    "os"
    "path/filepath"

    "ingress-cf-dns/config"

    "github.com/cloudflare/cloudflare-go/v4"
    "github.com/cloudflare/cloudflare-go/v4/dns"
    "github.com/cloudflare/cloudflare-go/v4/option"
    "github.com/cloudflare/cloudflare-go/v4/zones"
    corev1 "k8s.io/api/core/v1"
    networkingv1 "k8s.io/api/networking/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"
)

type Controller struct {
    kubeClient    *kubernetes.Clientset
    cfAPI         *cloudflare.Client
    config        *config.Config
}

func main() {
    // Load configuration
    cfg, err := config.LoadConfig()
    if err != nil {
        log.Fatalf("Error loading configuration: %s", err.Error())
    }

    // Create kubernetes client
    var kubeClient *kubernetes.Clientset

    // Try to use kubeconfig from user's home directory
    homeDir, err := os.UserHomeDir()
    if err != nil {
        log.Fatalf("Error getting user home directory: %s", err.Error())
    }
    kubeconfigPath := filepath.Join(homeDir, ".kube", "config")
    if _, err := os.Stat(kubeconfigPath); err == nil {
        // Use kubeconfig file from home directory
        config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
        if err != nil {
            log.Fatalf("Error building kubeconfig: %s", err.Error())
        }
        kubeClient, err = kubernetes.NewForConfig(config)
    } else {
        // Use in-cluster config
        config, err := rest.InClusterConfig()
        if err != nil {
            log.Fatalf("Error getting in-cluster config: %s", err.Error())
        }
        kubeClient, err = kubernetes.NewForConfig(config)

        if err != nil {
            log.Fatalf("Error building kubernetes client: %s", err.Error())
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
    log.Printf("Starting controller with sync interval: %v", c.config.SyncInterval)
    
    // Run first sync immediately
    c.syncAll()

    // Start periodic sync
    ticker := time.NewTicker(c.config.SyncInterval)
    for range ticker.C {
        c.syncAll()
    }
}

func (c *Controller) syncAll() {
    log.Println("Starting sync...")
    
    // Get all ingresses from all namespaces
    ingresses, err := c.kubeClient.NetworkingV1().Ingresses("").List(context.Background(), metav1.ListOptions{})
    if err != nil {
        log.Printf("Error listing ingresses: %s", err.Error())
        return
    }

    // Process each ingress
    for _, ing := range ingresses.Items {
        // Skip if namespace is not allowed
        if !c.config.IsNamespaceAllowed(ing.Namespace) {
            log.Printf("Skipping ingress %s/%s: namespace not allowed by pattern %s", 
                ing.Namespace, ing.Name, c.config.NamespaceRegex)
            continue
        }
        c.processIngress(&ing)
    }
    
    log.Println("Sync completed")
}

func (c *Controller) processIngress(ing *networkingv1.Ingress) {
    // Map to track host to backend mapping
    hostBackends := make(map[string]string) // host -> backend service name
    backendHosts := make(map[string][]string) // backend service name -> hosts

    // First pass: collect and validate host-backend mappings
    for _, rule := range ing.Spec.Rules {
        if rule.Host == "" || rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
            continue
        }

        // Skip if domain is not allowed
        if !c.config.IsDomainAllowed(rule.Host) {
            log.Printf("Skipping host %s in ingress %s/%s: domain not allowed by pattern %s", 
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
                log.Printf("Warning: Host %s already mapped to backend %s, skipping new backend %s", 
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
            log.Printf("Error getting service %s in namespace %s: %s", backendName, ing.Namespace, err.Error())
            continue
        }

        // Get pods for this service
        pods, err := c.getPodsForService(svc)
        if err != nil {
            log.Printf("Error getting pods for service %s in namespace %s: %s", svc.Name, svc.Namespace, err.Error())
            continue
        }

        // Skip if no pods found
        if len(pods.Items) == 0 {
            log.Printf("No pods found for service %s in namespace %s", svc.Name, svc.Namespace)
            continue
        }

        // Warn if service has multiple pods
        if len(pods.Items) > 1 {
            log.Printf("Warning: Service %s in namespace %s has %d pods, using pod %s for DNS record", 
                svc.Name, svc.Namespace, len(pods.Items), pods.Items[0].Name)
        }

        // Get the first pod's node
        pod := pods.Items[0]
        if pod.Spec.NodeName == "" {
            log.Printf("Pod %s has no node assigned", pod.Name)
            continue
        }

        // Get node
        node, err := c.kubeClient.CoreV1().Nodes().Get(context.Background(), pod.Spec.NodeName, metav1.GetOptions{})
        if err != nil {
            log.Printf("Error getting node %s: %s", pod.Spec.NodeName, err.Error())
            continue
        }

        // Get public IP from node annotation
        publicIP, ok := node.Annotations[c.config.NodePublicIPAnnotation]
        if !ok {
            log.Printf("Node %s does not have public IP annotation", node.Name)
            continue
        }

        // Update DNS records for all hosts of this backend
        for _, host := range hosts {
            c.updateDNSRecord(host, publicIP)
        }
    }
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
            log.Printf("Error listing zones: %s", err.Error())
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

func (c *Controller) updateDNSRecord(host, publicIP string) {
    // Get Zone ID for the host
    zoneID, err := c.getZoneIDByDomain(host)
    if err != nil {
        log.Printf("Error getting zone ID for host %s: %s", host, err.Error())
        return
    }

    ctx := context.Background()
    
    // Check if record exists
    listParams := dns.RecordListParams{
        ZoneID: cloudflare.F(zoneID),
        Name: cloudflare.F(dns.RecordListParamsName{
            Exact: cloudflare.F(host),
        }),
        // Type: cloudflare.F(dns.RecordListParamsTypeA),
    }
    
    records, err := c.cfAPI.DNS.Records.List(ctx, listParams)
    if err != nil {
        log.Printf("Error checking DNS record: %s", err.Error())
        return
    }

    if len(records.Result) > 0 {
        // Check if update is needed
        record := records.Result[0]

        // Check if record is managed by another controller
        if strings.HasPrefix(strings.ToLower(record.Comment), "managed by") && record.Comment != config.DNSRecordComment {
            log.Printf("Warning: DNS record for %s is managed by another controller (comment: %s), skipping update", 
                host, record.Comment)
            return
        }

        if record.Content == publicIP {
            log.Printf("DNS record for %s already points to %s in zone ID %s, skipping update", host, publicIP, zoneID)
            return
        }

        // Update existing record
        updateParams := dns.RecordUpdateParams{
            ZoneID: cloudflare.F(zoneID),
            Record: dns.ARecordParam{
                Name:    cloudflare.F(host),
                Type:    cloudflare.F(dns.ARecordTypeA),
                Content: cloudflare.F(publicIP),
                TTL:     cloudflare.F(dns.TTL1),
                Comment: cloudflare.F(config.DNSRecordComment),
            },
        }
        _, err = c.cfAPI.DNS.Records.Update(ctx, record.ID, updateParams)
        if err != nil {
            log.Printf("Error updating DNS record for %s from %s to %s in zone ID %s: %s", 
                host, record.Content, publicIP, zoneID, err.Error())
        } else {
            log.Printf("Successfully updated DNS record for %s from %s to %s in zone ID %s (proxied: %v)", 
                host, record.Content, publicIP, zoneID, c.config.DNSProxied)
        }
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
            log.Printf("Error creating DNS record for %s to %s in zone ID %s: %s", 
                host, publicIP, zoneID, err.Error())
        } else {
            log.Printf("Successfully created DNS record for %s to %s in zone ID %s (proxied: %v)", 
                host, publicIP, zoneID, c.config.DNSProxied)
        }
    }
} 