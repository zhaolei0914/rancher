package logging

import (
	"bytes"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/rancher/norman/condition"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/parse"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/rancher/pkg/clustermanager"
	loggingconfig "github.com/rancher/rancher/pkg/controllers/user/logging/config"
	"github.com/rancher/rancher/pkg/controllers/user/logging/configsyncer"
	"github.com/rancher/rancher/pkg/controllers/user/logging/deployer"
	"github.com/rancher/rancher/pkg/controllers/user/logging/utils"
	"github.com/rancher/rancher/pkg/ref"
	mgmtv3 "github.com/rancher/types/apis/management.cattle.io/v3"
	projectv3 "github.com/rancher/types/apis/project.cattle.io/v3"
	mgmtv3client "github.com/rancher/types/client/management/v3"
	"github.com/rancher/types/config"
	"github.com/rancher/types/config/dialer"

	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	k8scorev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	fluentdCommand      = "fluentd -q --dry-run -i "
	cleanCertDirCommand = "rm -rf "
	tmpCertDirPrefix    = "/fluentd/etc/config/tmpCert"
)

type Handler struct {
	clusterManager       *clustermanager.Manager
	dialerFactory        dialer.Factory
	appsGetter           projectv3.AppsGetter
	projectLister        mgmtv3.ProjectLister
	templateLister       mgmtv3.CatalogTemplateLister
	projectLoggingLister mgmtv3.ProjectLoggingLister
}

func NewHandler(management *config.ScaledContext, clusterManager *clustermanager.Manager) *Handler {
	return &Handler{
		clusterManager:       clusterManager,
		dialerFactory:        management.Dialer,
		appsGetter:           management.Project,
		projectLister:        management.Management.Projects("").Controller().Lister(),
		templateLister:       management.Management.CatalogTemplates("").Controller().Lister(),
		projectLoggingLister: management.Management.ProjectLoggings("").Controller().Lister(),
	}
}

func CollectionFormatter(apiContext *types.APIContext, resource *types.GenericCollection) {
	resource.AddAction(apiContext, "test")
	resource.AddAction(apiContext, "dryRun")
}

func (h *Handler) ActionHandler(actionName string, action *types.Action, apiContext *types.APIContext) error {
	var target mgmtv3.LoggingTargets
	var clusterName, projectID, level, containerLogSourceTag string
	var outputTags map[string]string

	switch apiContext.Type {
	case mgmtv3client.ClusterLoggingType:
		if !canPerformAction(apiContext, mgmtv3.ClusterLoggingGroupVersionKind.Group, mgmtv3.ClusterLoggingResource.Name) {
			return httperror.NewAPIError(httperror.NotFound, "not found")
		}

		var input mgmtv3.ClusterTestInput
		actionInput, err := parse.ReadBody(apiContext.Request)
		if err != nil {
			return err
		}

		if err = convert.ToObj(actionInput, &input); err != nil {
			return err
		}

		target = input.LoggingTargets
		clusterName = input.ClusterName
		level = loggingconfig.ClusterLevel
		containerLogSourceTag = level
		outputTags = input.OutputTags

	case mgmtv3client.ProjectLoggingType:
		if !canPerformAction(apiContext, mgmtv3.ProjectLoggingGroupVersionKind.Group, mgmtv3.ProjectLoggingResource.Name) {
			return httperror.NewAPIError(httperror.NotFound, "not found")
		}

		var input mgmtv3.ProjectTestInput
		actionInput, err := parse.ReadBody(apiContext.Request)
		if err != nil {
			return err
		}

		if err = convert.ToObj(actionInput, &input); err != nil {
			return err
		}

		target = input.LoggingTargets
		projectID = input.ProjectName
		clusterName, _ = ref.Parse(input.ProjectName)
		level = loggingconfig.ProjectLevel
		containerLogSourceTag = projectID
		outputTags = input.OutputTags
	}

	if err := validate(level, containerLogSourceTag, target, outputTags); err != nil {
		return err
	}

	switch actionName {
	case "test":
		if err := h.testLoggingTarget(clusterName, target); err != nil {
			return httperror.NewAPIError(httperror.ServerError, err.Error())
		}

		apiContext.WriteResponse(http.StatusNoContent, nil)
		return nil

	case "dryRun":

		if err := h.dryRunLoggingTarget(apiContext, level, clusterName, projectID, target); err != nil {
			return httperror.NewAPIError(httperror.ServerError, err.Error())
		}

		apiContext.WriteResponse(http.StatusNoContent, nil)
		return nil

	}

	return httperror.NewAPIError(httperror.InvalidAction, "invalid action: "+actionName)

}

func (h *Handler) testLoggingTarget(clusterName string, target mgmtv3.LoggingTargets) error {
	clusterDialer, err := h.dialerFactory.ClusterDialer(clusterName)
	if err != nil {
		return errors.Wrap(err, "get cluster dialer failed")
	}

	wp := utils.NewLoggingTargetTestWrap(target)
	if wp == nil {
		return nil
	}

	return wp.TestReachable(clusterDialer, true)
}

func (h *Handler) dryRunLoggingTarget(apiContext *types.APIContext, level, clusterName, projectID string, target mgmtv3.LoggingTargets) error {
	context, err := h.clusterManager.UserContext(clusterName)
	if err != nil {
		return err
	}

	podLister := context.Core.Pods(loggingconfig.LoggingNamespace).Controller().Lister()
	namespaces := context.Core.Namespaces(metav1.NamespaceAll)
	testerDeployer := deployer.NewTesterDeployer(context.Management, h.appsGetter, h.projectLister, podLister, h.projectLoggingLister, namespaces, h.templateLister)
	configGenerator := configsyncer.NewConfigGenerator(metav1.NamespaceAll, h.projectLoggingLister, namespaces.Controller().Lister())

	var dryRunConfigBuf []byte
	var certificate, clientCert, clientKey, certificatePath, clientCertPath, clientKeyPath, certScretKeyName string

	tmpCertDir := fmt.Sprintf("%s/%s", tmpCertDirPrefix, uuid.NewV4().String())
	if level == loggingconfig.ClusterLevel {
		clusterLogging := &mgmtv3.ClusterLogging{
			Spec: mgmtv3.ClusterLoggingSpec{
				LoggingTargets: target,
				ClusterName:    clusterName,
			},
		}
		dryRunConfigBuf, err = configGenerator.GenerateClusterLoggingConfig(clusterLogging, "testSystemProjectID", tmpCertDir)
		if err != nil {
			return err
		}

		certificate, clientCert, clientKey = configsyncer.GetSSLConfig(clusterLogging.Spec.LoggingTargets)
		certScretKeyName = clusterName
	} else {
		new := &mgmtv3.ProjectLogging{
			Spec: mgmtv3.ProjectLoggingSpec{
				LoggingTargets: target,
				ProjectName:    projectID,
			},
		}

		dryRunConfigBuf, err = configGenerator.GenerateProjectLoggingConfig([]*mgmtv3.ProjectLogging{new}, "testSystemProjectID", tmpCertDir)
		if err != nil {
			return err
		}

		certificate, clientCert, clientKey = configsyncer.GetSSLConfig(new.Spec.LoggingTargets)
		certScretKeyName = strings.Replace(projectID, ":", "_", -1)
	}

	certificatePath = addCertPrefixPath(tmpCertDir, loggingconfig.SecretDataKeyCa(level, certScretKeyName))
	clientCertPath = addCertPrefixPath(tmpCertDir, loggingconfig.SecretDataKeyCert(level, certScretKeyName))
	clientKeyPath = addCertPrefixPath(tmpCertDir, loggingconfig.SecretDataKeyCertKey(level, certScretKeyName))

	if err := testerDeployer.Deploy(level, clusterName, projectID, target); err != nil {
		return err
	}

	testerPods, err := context.K8sClient.CoreV1().Pods(loggingconfig.LoggingNamespace).List(metav1.ListOptions{
		LabelSelector: labels.Set(loggingconfig.FluentdTesterSelector).String(),
	})
	if err != nil {
		return err
	}

	if len(testerPods.Items) == 0 {
		return errors.New("could not find fluentd tester pod")
	}

	testerPod := testerPods.Items[0]
	if !condition.Cond(k8scorev1.PodReady).IsTrue(&testerPod) {
		return errors.New("pod fluentd tester not ready")
	}

	req := context.K8sClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(testerPod.Name).
		Namespace(testerPod.Namespace).
		SubResource("exec").
		VersionedParams(&k8scorev1.PodExecOptions{
			Container: loggingconfig.FluentdTesterContainerName,
			Command:   []string{"bash"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(&context.RESTConfig, "POST", req.URL())
	if err != nil {
		return errors.Wrap(err, "create executor failed")
	}

	var testCmd bytes.Buffer
	testCmd.WriteString("mkdir -p " + tmpCertDir)

	if certificate != "" {
		testCmd.WriteString(` && echo "` + certificate + `" >> ` + certificatePath)
	}
	if clientCert != "" {
		testCmd.WriteString(` && echo "` + clientCert + `" >> ` + clientCertPath)
	}
	if clientKey != "" {
		testCmd.WriteString(` && echo "` + clientKey + `" >> ` + clientKeyPath)
	}

	replacer := strings.NewReplacer(loggingconfig.DefaultCertDir, tmpCertDir, `$`, `\$`)
	dryRunConfig := replacer.Replace(string(dryRunConfigBuf))
	testCmd.WriteString(` && ` + fluentdCommand)
	testCmd.WriteString(fmt.Sprintf(`"%s"`, dryRunConfig))

	testCmd.WriteString(` && ` + cleanCertDirCommand + tmpCertDir)
	return remoteExcuteCommand(executor, testCmd)
}

func remoteExcuteCommand(executor remotecommand.Executor, stdin bytes.Buffer) error {
	var stdout, stderr bytes.Buffer
	handler := &streamHandler{resizeEvent: make(chan remotecommand.TerminalSize)}

	if err := executor.Stream(remotecommand.StreamOptions{
		Stdin:             &stdin,
		Stdout:            &stdout,
		Stderr:            &stderr,
		TerminalSizeQueue: handler,
		Tty:               true,
	}); err != nil {
		err = errors.Wrapf(err, "stream error")
		if stderr.String() != "" {
			err = errors.WithMessage(err, "stderr: "+stderr.String())
		}

		if stdout.String() != "" {
			err = errors.WithMessage(err, "stdout: "+stdout.String())
		}
		return err
	}

	if stderr.String() != "" {
		return errors.New("dry run fluentd failed, stderr: " + stderr.String())
	}

	if stdout.String() != "" {
		return errors.New("dry run fluentd failed, stdout: " + stdout.String())
	}

	return nil
}

func (handler *streamHandler) Next() (size *remotecommand.TerminalSize) {
	ret := <-handler.resizeEvent
	size = &ret
	return
}

type streamHandler struct {
	resizeEvent chan remotecommand.TerminalSize
}

func addCertPrefixPath(certDir, file string) string {
	return path.Join(certDir, file)
}

func canPerformAction(apiContext *types.APIContext, groupName, resourceName string) bool {
	alertObj := map[string]interface{}{
		"id": apiContext.ID,
	}
	return apiContext.AccessControl.CanDo(groupName, resourceName, "create", apiContext, alertObj, apiContext.Schema) == nil
}
