package kubernetes

// The subset of the Kubernetes API this runtime reads and writes.

type meta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []ownerRef        `json:"ownerReferences,omitempty"`
}

type ownerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type pod struct {
	APIVersion string    `json:"apiVersion,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Metadata   meta      `json:"metadata"`
	Spec       podSpec   `json:"spec"`
	Status     podStatus `json:"status,omitzero"`
}

type podSpec struct {
	RestartPolicy      string          `json:"restartPolicy"`
	ServiceAccountName string          `json:"serviceAccountName,omitempty"`
	AutomountToken     *bool           `json:"automountServiceAccountToken"`
	EnableServiceLinks *bool           `json:"enableServiceLinks"`
	ActiveDeadline     *int64          `json:"activeDeadlineSeconds,omitempty"`
	TerminationGrace   *int64          `json:"terminationGracePeriodSeconds,omitempty"`
	SecurityContext    podSecurity     `json:"securityContext"`
	Containers         []containerSpec `json:"containers"`
	Volumes            []volume        `json:"volumes,omitempty"`
}

type podSecurity struct {
	RunAsNonRoot        *bool    `json:"runAsNonRoot,omitempty"`
	RunAsUser           *int64   `json:"runAsUser,omitempty"`
	RunAsGroup          *int64   `json:"runAsGroup,omitempty"`
	FSGroup             *int64   `json:"fsGroup,omitempty"`
	FSGroupChangePolicy string   `json:"fsGroupChangePolicy,omitempty"`
	Seccomp             *seccomp `json:"seccompProfile,omitempty"`
}

type seccomp struct {
	Type string `json:"type"`
}

type containerSpec struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Command         []string          `json:"command"`
	WorkingDir      string            `json:"workingDir,omitempty"`
	ImagePullPolicy string            `json:"imagePullPolicy,omitempty"`
	Env             []envVar          `json:"env,omitempty"`
	Resources       *resources        `json:"resources,omitempty"`
	VolumeMounts    []volumeMount     `json:"volumeMounts,omitempty"`
	SecurityContext containerSecurity `json:"securityContext"`
}

type containerSecurity struct {
	AllowPrivilegeEscalation *bool         `json:"allowPrivilegeEscalation"`
	Privileged               *bool         `json:"privileged"`
	ReadOnlyRoot             bool          `json:"readOnlyRootFilesystem"`
	Capabilities             *capabilities `json:"capabilities"`
}

type capabilities struct {
	Drop []string `json:"drop"`
}

type envVar struct {
	Name      string     `json:"name"`
	Value     string     `json:"value,omitempty"`
	ValueFrom *envSource `json:"valueFrom,omitempty"`
}

type envSource struct {
	SecretKeyRef *keyRef `json:"secretKeyRef,omitempty"`
}

type keyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type resources struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type volume struct {
	Name     string     `json:"name"`
	EmptyDir *emptyDir  `json:"emptyDir,omitempty"`
	PVC      *pvcSource `json:"persistentVolumeClaim,omitempty"`
}

type emptyDir struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type pvcSource struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type podStatus struct {
	Phase             string            `json:"phase,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	ContainerStatuses []containerStatus `json:"containerStatuses,omitempty"`
}

type containerStatus struct {
	Name  string `json:"name"`
	State struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting,omitempty"`
		Running *struct {
			StartedAt string `json:"startedAt"`
		} `json:"running,omitempty"`
		Terminated *struct {
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason"`
		} `json:"terminated,omitempty"`
	} `json:"state"`
}
