package jobs

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	labelJobType = "re-tools/job-type"
	labelJobID   = "re-tools/job-id"
	labelTool    = "re-tools/tool"
	jobTypeValue = "analysis"
	namespace    = "re-tools"
	pvcName      = "re-tools-data"
)

// JobEventHandler is called when a Kubernetes Job changes state.
type JobEventHandler interface {
	OnJobComplete(jobID uuid.UUID, tool string)
	OnJobFailed(jobID uuid.UUID, tool string, logs string)
}

// Manager creates and watches Kubernetes Jobs for APK analysis.
type Manager struct {
	clientset    *kubernetes.Clientset
	namespace    string
	jadxImage    string
	apktoolImage string
	pvcName      string
}

// NewManager creates a Manager using in-cluster config and the given image refs.
func NewManager(jadxImage, apktoolImage string) (*Manager, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientset: %w", err)
	}

	return &Manager{
		clientset:    clientset,
		namespace:    namespace,
		jadxImage:    jadxImage,
		apktoolImage: apktoolImage,
		pvcName:      pvcName,
	}, nil
}

// CreateAnalysisJobs launches jadx and apktool Kubernetes Jobs for the given APK.
func (m *Manager) CreateAnalysisJobs(ctx context.Context, jobID uuid.UUID, apkPath string) error {
	outputBase := fmt.Sprintf("/data/output/%s", jobID.String())

	jadxJob := m.buildJob(jobID, "jadx", m.jadxImage,
		[]string{"--apk", apkPath, "--output", outputBase + "/jadx"},
		corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
			corev1.ResourceCPU:    resource.MustParse("500m"),
		},
		corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			corev1.ResourceCPU:    resource.MustParse("2"),
		},
	)

	apktoolJob := m.buildJob(jobID, "apktool", m.apktoolImage,
		[]string{"--apk", apkPath, "--output", outputBase + "/apktool"},
		corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceCPU:    resource.MustParse("500m"),
		},
		corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceCPU:    resource.MustParse("500m"),
		},
	)

	if _, err := m.clientset.BatchV1().Jobs(m.namespace).Create(ctx, jadxJob, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create jadx job: %w", err)
	}

	if _, err := m.clientset.BatchV1().Jobs(m.namespace).Create(ctx, apktoolJob, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create apktool job: %w", err)
	}

	return nil
}

func (m *Manager) buildJob(jobID uuid.UUID, tool, image string, args []string, requests, limits corev1.ResourceList) *batchv1.Job {
	ttl := int32(3600)
	backoff := int32(1)
	deadline := int64(600)
	runAsNonRoot := true
	runAsUser := int64(1000)
	allowPrivilegeEscalation := false

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("re-%s-%s", tool, jobID.String()),
			Namespace: m.namespace,
			Labels: map[string]string{
				labelJobType: jobTypeValue,
				labelJobID:   jobID.String(),
				labelTool:    tool,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labelJobType: jobTypeValue,
						labelJobID:   jobID.String(),
						labelTool:    tool,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					ImagePullSecrets: []corev1.LocalObjectReference{
						{Name: "ghcr-pull-secret"},
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: m.pvcName,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  tool,
							Image: image,
							Args:  args,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &allowPrivilegeEscalation,
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: requests,
								Limits:   limits,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/data",
								},
							},
						},
					},
				},
			},
		},
	}
}

// WatchJobs uses a SharedInformer to watch analysis Jobs and notify the handler.
func (m *Manager) WatchJobs(ctx context.Context, handler JobEventHandler) {
	labelSelector := labels.SelectorFromSet(labels.Set{
		labelJobType: jobTypeValue,
	})

	factory := informers.NewSharedInformerFactoryWithOptions(
		m.clientset,
		30*time.Second,
		informers.WithNamespace(m.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labelSelector.String()
		}),
	)

	jobInformer := factory.Batch().V1().Jobs().Informer()

	jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			job, ok := newObj.(*batchv1.Job)
			if !ok {
				return
			}
			m.handleJobUpdate(ctx, job, handler)
		},
		AddFunc: func(obj interface{}) {
			job, ok := obj.(*batchv1.Job)
			if !ok {
				return
			}
			m.handleJobUpdate(ctx, job, handler)
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	<-ctx.Done()
}

func (m *Manager) handleJobUpdate(ctx context.Context, job *batchv1.Job, handler JobEventHandler) {
	jobIDStr, ok := job.Labels[labelJobID]
	if !ok {
		return
	}
	tool, ok := job.Labels[labelTool]
	if !ok {
		return
	}

	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		log.Printf("invalid job-id label %q: %v", jobIDStr, err)
		return
	}

	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			handler.OnJobComplete(jobID, tool)
			return
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			logs := m.fetchPodLogs(ctx, job)
			handler.OnJobFailed(jobID, tool, logs)
			return
		}
	}
}

func (m *Manager) fetchPodLogs(ctx context.Context, job *batchv1.Job) string {
	selector := labels.SelectorFromSet(job.Spec.Selector.MatchLabels)
	pods, err := m.clientset.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Sprintf("(error listing pods: %v)", err)
	}
	if len(pods.Items) == 0 {
		return "(no pods found)"
	}

	pod := pods.Items[0]
	containerName := ""
	if len(pod.Spec.Containers) > 0 {
		containerName = pod.Spec.Containers[0].Name
	}

	req := m.clientset.CoreV1().Pods(m.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: containerName,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return fmt.Sprintf("(error streaming logs: %v)", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Sprintf("(error reading logs: %v)", err)
	}
	return string(data)
}

// ReconcileRunningJobs checks Kubernetes state for jobs the DB thinks are running.
func (m *Manager) ReconcileRunningJobs(ctx context.Context, runningJobs []uuid.UUID, handler JobEventHandler) error {
	for _, jobID := range runningJobs {
		for _, tool := range []string{"jadx", "apktool"} {
			k8sJobName := fmt.Sprintf("re-%s-%s", tool, jobID.String())
			k8sJob, err := m.clientset.BatchV1().Jobs(m.namespace).Get(ctx, k8sJobName, metav1.GetOptions{})
			if err != nil {
				// Job not found — mark as failed
				log.Printf("reconcile: k8s job %s not found, marking failed: %v", k8sJobName, err)
				handler.OnJobFailed(jobID, tool, "job not found after coordinator restart")
				continue
			}

			for _, cond := range k8sJob.Status.Conditions {
				if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
					handler.OnJobComplete(jobID, tool)
				} else if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
					logs := m.fetchPodLogs(ctx, k8sJob)
					handler.OnJobFailed(jobID, tool, logs)
				}
			}
		}
	}
	return nil
}

