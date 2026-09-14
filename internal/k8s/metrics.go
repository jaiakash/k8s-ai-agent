package k8s

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceUsage is one row of a top_pods / top_nodes listing.
type ResourceUsage struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	// CPUPercent and MemoryPercent are populated for nodes, where a capacity
	// is known. A raw quantity means little without the denominator.
	CPUPercent    string `json:"cpuPercent,omitempty"`
	MemoryPercent string `json:"memoryPercent,omitempty"`
}

// errNoMetrics explains the most common cause rather than surfacing a 404.
func errNoMetrics(err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("the metrics API is not available in this cluster: " +
			"metrics-server is most likely not installed (https://github.com/kubernetes-sigs/metrics-server)")
	}
	return fmt.Errorf("read metrics API: %w", err)
}

// TopPods returns pod CPU and memory usage, highest CPU first.
func (c *Client) TopPods(ctx context.Context, ns string, limit int) ([]ResourceUsage, error) {
	list, err := c.Metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errNoMetrics(err)
	}

	out := make([]ResourceUsage, 0, len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]

		// A pod's usage is the sum across its containers.
		cpu := resource.NewQuantity(0, resource.DecimalSI)
		mem := resource.NewQuantity(0, resource.BinarySI)
		for _, ctr := range m.Containers {
			if q, ok := ctr.Usage[corev1.ResourceCPU]; ok {
				cpu.Add(q)
			}
			if q, ok := ctr.Usage[corev1.ResourceMemory]; ok {
				mem.Add(q)
			}
		}

		out = append(out, ResourceUsage{
			Name:      m.Name,
			Namespace: m.Namespace,
			CPU:       formatCPU(cpu),
			Memory:    formatMemory(mem),
		})
	}

	sortByCPUDesc(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TopNodes returns node usage alongside each node's capacity.
func (c *Client) TopNodes(ctx context.Context) ([]ResourceUsage, error) {
	list, err := c.Metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errNoMetrics(err)
	}

	// Capacity comes from the Node objects, not the metrics API, so the
	// percentages below need both. Missing capacity degrades to raw figures.
	capacity := map[string]corev1.ResourceList{}
	if nodes, err := c.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for i := range nodes.Items {
			capacity[nodes.Items[i].Name] = nodes.Items[i].Status.Allocatable
		}
	}

	out := make([]ResourceUsage, 0, len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]

		cpu := m.Usage[corev1.ResourceCPU]
		mem := m.Usage[corev1.ResourceMemory]

		u := ResourceUsage{
			Name:   m.Name,
			CPU:    formatCPU(&cpu),
			Memory: formatMemory(&mem),
		}
		if alloc, ok := capacity[m.Name]; ok {
			if c, ok := alloc[corev1.ResourceCPU]; ok && c.MilliValue() > 0 {
				u.CPUPercent = fmt.Sprintf("%d%%", cpu.MilliValue()*100/c.MilliValue())
			}
			if c, ok := alloc[corev1.ResourceMemory]; ok && c.Value() > 0 {
				u.MemoryPercent = fmt.Sprintf("%d%%", mem.Value()*100/c.Value())
			}
		}
		out = append(out, u)
	}

	sortByCPUDesc(out)
	return out, nil
}

// sortByCPUDesc orders rows by raw CPU millivalue, descending. The formatted
// strings sort wrongly ("9m" > "10m" lexically), so parse them back.
func sortByCPUDesc(rows []ResourceUsage) {
	sort.SliceStable(rows, func(i, j int) bool {
		qi, erri := resource.ParseQuantity(rows[i].CPU)
		qj, errj := resource.ParseQuantity(rows[j].CPU)
		if erri != nil || errj != nil {
			return rows[i].Name < rows[j].Name
		}
		return qi.MilliValue() > qj.MilliValue()
	})
}

// formatCPU renders CPU in millicores, matching `kubectl top`.
func formatCPU(q *resource.Quantity) string {
	return fmt.Sprintf("%dm", q.MilliValue())
}

// formatMemory renders memory in mebibytes, matching `kubectl top`.
func formatMemory(q *resource.Quantity) string {
	return fmt.Sprintf("%dMi", q.Value()/(1024*1024))
}
