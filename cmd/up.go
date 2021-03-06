package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/covexo/devspace/pkg/util/yamlutil"

	"github.com/covexo/devspace/pkg/devspace/builder"

	"github.com/docker/docker/api/types"

	"github.com/covexo/devspace/pkg/devspace/builder/docker"

	"github.com/covexo/devspace/pkg/devspace/config/v1"

	"github.com/covexo/devspace/pkg/util/randutil"

	"github.com/covexo/devspace/pkg/util/log"

	"github.com/covexo/devspace/pkg/devspace/builder/kaniko"
	"github.com/covexo/devspace/pkg/devspace/registry"
	synctool "github.com/covexo/devspace/pkg/devspace/sync"

	helmClient "github.com/covexo/devspace/pkg/devspace/clients/helm"
	"github.com/covexo/devspace/pkg/devspace/clients/kubectl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/covexo/devspace/pkg/devspace/config/configutil"
	"github.com/spf13/cobra"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/exec"
)

// UpCmd is a struct that defines a command call for "up"
type UpCmd struct {
	flags     *UpCmdFlags
	helm      *helmClient.HelmClientWrapper
	kubectl   *kubernetes.Clientset
	workdir   string
	pod       *k8sv1.Pod
	container *k8sv1.Container
}

// UpCmdFlags are the flags available for the up-command
type UpCmdFlags struct {
	tiller         bool
	open           string
	initRegistries bool
	build          bool
	shell          string
	sync           bool
	deploy         bool
	portforwarding bool
	noSleep        bool
}

//UpFlagsDefault are the default flags for UpCmdFlags
var UpFlagsDefault = &UpCmdFlags{
	tiller:         true,
	open:           "cmd",
	initRegistries: true,
	build:          true,
	sync:           true,
	deploy:         false,
	portforwarding: true,
	noSleep:        false,
}

func init() {
	cmd := &UpCmd{
		flags: UpFlagsDefault,
	}

	cobraCmd := &cobra.Command{
		Use:   "up",
		Short: "Starts your DevSpace",
		Long: `
#######################################################
#################### devspace up ######################
#######################################################
Starts and connects your DevSpace:
1. Connects to the Tiller server
2. Builds your Docker image (if your Dockerfile has changed)
3. Deploys the Helm chart in /chart
4. Starts the sync client
5. Enters the container shell
#######################################################`,
		Args: cobra.NoArgs,
		Run:  cmd.Run,
	}
	rootCmd.AddCommand(cobraCmd)

	cobraCmd.Flags().BoolVar(&cmd.flags.tiller, "tiller", cmd.flags.tiller, "Install/upgrade tiller")
	cobraCmd.Flags().BoolVar(&cmd.flags.initRegistries, "init-registries", cmd.flags.initRegistries, "Initialize registries (and install internal one)")
	cobraCmd.Flags().BoolVarP(&cmd.flags.build, "build", "b", cmd.flags.build, "Build image if Dockerfile has been modified")
	cobraCmd.Flags().StringVarP(&cmd.flags.shell, "shell", "s", "", "Shell command (default: bash, fallback: sh)")
	cobraCmd.Flags().BoolVar(&cmd.flags.sync, "sync", cmd.flags.sync, "Enable code synchronization")
	cobraCmd.Flags().BoolVar(&cmd.flags.portforwarding, "portforwarding", cmd.flags.portforwarding, "Enable port forwarding")
	cobraCmd.Flags().BoolVarP(&cmd.flags.deploy, "deploy", "d", cmd.flags.deploy, "Deploy chart")
	cobraCmd.Flags().BoolVar(&cmd.flags.noSleep, "no-sleep", cmd.flags.noSleep, "Enable no-sleep")
}

// Run executes the command logic
func (cmd *UpCmd) Run(cobraCmd *cobra.Command, args []string) {
	log.StartFileLogging()

	workdir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Unable to determine current workdir: %s", err.Error())
	}

	cmd.workdir = workdir

	configExists, _ := configutil.ConfigExists()
	if !configExists {
		initCmd := &InitCmd{
			flags: InitCmdFlagsDefault,
		}

		initCmd.Run(nil, []string{})
	}

	cmd.kubectl, err = kubectl.NewClient()
	if err != nil {
		log.Fatalf("Unable to create new kubectl client: %v", err)
	}

	err = cmd.ensureNamespace()
	if err != nil {
		log.Fatalf("Unable to create release namespace: %v", err)
	}

	cmd.initHelm()

	if cmd.flags.initRegistries {
		cmd.initRegistries()
	}
	mustRedeploy := false

	if cmd.flags.build {
		mustRedeploy = cmd.buildImages(cobraCmd.Flags().Changed("build"))
	}

	// Check if we find a running release pod
	pod, err := getRunningDevSpacePod(cmd.helm, cmd.kubectl)

	if err != nil || mustRedeploy || cmd.flags.deploy {
		cmd.deployChart()
	} else {
		cmd.pod = pod
	}

	if cmd.flags.portforwarding {
		cmd.startPortForwarding()
	}

	if cmd.flags.sync {
		syncConfigs := cmd.startSync()
		defer func() {
			for _, v := range syncConfigs {
				v.Stop()
			}
		}()
	}

	cmd.enterTerminal()
}

func (cmd *UpCmd) ensureNamespace() error {
	config := configutil.GetConfig(false)
	releaseNamespace := *config.DevSpace.Release.Namespace

	// Check if registry namespace exists
	_, err := cmd.kubectl.CoreV1().Namespaces().Get(releaseNamespace, metav1.GetOptions{})
	if err != nil {
		// Create registry namespace
		_, err = cmd.kubectl.CoreV1().Namespaces().Create(&k8sv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: releaseNamespace,
			},
		})

		if err != nil {
			return err
		}
	}

	return nil
}

func (cmd *UpCmd) initRegistries() {
	config := configutil.GetConfig(false)
	registryMap := *config.Registries

	if config.Services.InternalRegistry != nil {
		registryConf, regConfExists := registryMap["internal"]
		if !regConfExists {
			log.Fatal("Registry config not found for internal registry")
		}

		log.StartWait("Initializing internal registry")
		err := registry.InitInternalRegistry(cmd.kubectl, cmd.helm, config.Services.InternalRegistry, registryConf)
		log.StopWait()

		if err != nil {
			log.Fatalf("Internal registry error: %v", err)
		}

		err = configutil.SaveConfig()
		if err != nil {
			log.Fatalf("Saving config error: %v", err)
		}

		log.Done("Internal registry started")
	}

	for registryName, registryConf := range registryMap {
		if registryConf.Auth != nil && registryConf.Auth.Password != nil {
			username := ""
			password := *registryConf.Auth.Password
			email := "noreply@devspace-cloud.com"
			registryURL := ""

			if registryConf.Auth.Username != nil {
				username = *registryConf.Auth.Username
			}
			if registryConf.URL != nil {
				registryURL = *registryConf.URL
			}

			log.StartWait("Creating image pull secret for registry: " + registryName)
			err := registry.CreatePullSecret(cmd.kubectl, *config.DevSpace.Release.Namespace, registryURL, username, password, email)
			log.StopWait()

			if err != nil {
				log.Fatalf("Failed to create pull secret for registry: %v", err)
			}
		}
	}
}

func (cmd *UpCmd) shouldRebuild(imageConf *v1.ImageConfig, dockerfilePath string, buildFlagChanged bool) bool {
	var dockerfileModTime time.Time

	mustRebuild := true
	dockerfileInfo, err := os.Stat(dockerfilePath)

	if err != nil {
		if imageConf.Build.LatestTimestamp == nil {
			log.Fatalf("Dockerfile missing: %v", err)
		} else {
			mustRebuild = false
		}
	} else {
		dockerfileModTime = dockerfileInfo.ModTime()

		// When user has not used -b or --build flags
		if buildFlagChanged == false {
			if imageConf.Build.LatestTimestamp != nil {
				latestBuildTime, _ := time.Parse(time.RFC3339Nano, *imageConf.Build.LatestTimestamp)

				// only rebuild Docker image when Dockerfile has changed since latest build
				mustRebuild = (latestBuildTime.Equal(dockerfileModTime) == false)
			}
		}
	}
	imageConf.Build.LatestTimestamp = configutil.String(dockerfileModTime.Format(time.RFC3339Nano))

	return mustRebuild
}

// returns true when one of the images had to be rebuild
func (cmd *UpCmd) buildImages(buildFlagChanged bool) bool {
	re := false
	config := configutil.GetConfig(false)

	for imageName, imageConf := range *config.Images {
		dockerfilePath := "./Dockerfile"
		contextPath := "./"

		if imageConf.Build.DockerfilePath != nil {
			dockerfilePath = *imageConf.Build.DockerfilePath
		}

		if imageConf.Build.ContextPath != nil {
			contextPath = *imageConf.Build.ContextPath
		}
		dockerfilePath = filepath.Join(cmd.workdir, strings.TrimPrefix(dockerfilePath, "."))
		contextPath = filepath.Join(cmd.workdir, strings.TrimPrefix(contextPath, "."))

		if cmd.shouldRebuild(imageConf, dockerfilePath, buildFlagChanged) {
			re = true
			imageTag, randErr := randutil.GenerateRandomString(7)

			if randErr != nil {
				log.Fatalf("Image building failed: %s", randErr.Error())
			}
			registryConf, err := registry.GetRegistryConfig(imageConf)
			if err != nil {
				log.Fatal(err)
			}

			var imageBuilder builder.Interface

			buildInfo := "Building image '%s' with engine '%s'"
			engineName := ""
			registryURL := ""

			if registryConf.URL != nil {
				registryURL = *registryConf.URL
			}
			if registryURL == "hub.docker.com" {
				registryURL = ""
			}

			if imageConf.Build.Engine.Kaniko != nil {
				engineName = "kaniko"
				buildNamespace := *config.DevSpace.Release.Namespace
				allowInsecurePush := false

				if imageConf.Build.Engine.Kaniko.Namespace != nil {
					buildNamespace = *imageConf.Build.Engine.Kaniko.Namespace
				}

				if registryConf.Insecure != nil {
					allowInsecurePush = *registryConf.Insecure
				}
				imageBuilder, err = kaniko.NewBuilder(registryURL, *imageConf.Name, imageTag, buildNamespace, cmd.kubectl, allowInsecurePush)
				if err != nil {
					log.Fatalf("Error creating kaniko builder: %v", err)
				}
			} else {
				engineName = "docker"
				preferMinikube := true

				if imageConf.Build.Engine.Docker.PreferMinikube != nil {
					preferMinikube = *imageConf.Build.Engine.Docker.PreferMinikube
				}

				imageBuilder, err = docker.NewBuilder(registryURL, *imageConf.Name, imageTag, preferMinikube)
				if err != nil {
					log.Fatalf("Error creating docker client: %v", err)
				}
			}

			log.Infof(buildInfo, imageName, engineName)

			username := ""
			password := ""

			if registryConf.URL != nil {
				registryURL = *registryConf.URL
			}
			if registryConf.Auth != nil {
				if registryConf.Auth.Username != nil {
					username = *registryConf.Auth.Username
				}

				if registryConf.Auth.Password != nil {
					password = *registryConf.Auth.Password
				}
			}

			log.StartWait("Authenticating (" + registryURL + ")")
			_, err = imageBuilder.Authenticate(username, password, len(username) == 0)
			log.StopWait()

			if err != nil {
				log.Fatalf("Error during image registry authentication: %v", err)
			}

			log.Done("Authentication successful (" + registryURL + ")")

			buildOptions := &types.ImageBuildOptions{}
			if imageConf.Build.Options != nil {
				if imageConf.Build.Options.BuildArgs != nil {
					buildOptions.BuildArgs = *imageConf.Build.Options.BuildArgs
				}
			}

			err = imageBuilder.BuildImage(contextPath, dockerfilePath, buildOptions)
			if err != nil {
				log.Fatalf("Error during image build: %v", err)
			}

			err = imageBuilder.PushImage()
			if err != nil {
				log.Fatalf("Error during image push: %v", err)
			}

			log.Info("Image pushed to registry (" + registryURL + ")")
			imageConf.Tag = &imageTag

			err = configutil.SaveConfig()
			if err != nil {
				log.Fatalf("Config saving error: %s", err.Error())
			}

			log.Done("Done building and pushing image '" + imageName + "'")
		} else {
			log.Infof("Skip building image '%s'", imageName)
		}
	}
	return re
}

func (cmd *UpCmd) initHelm() {
	if cmd.helm == nil {
		log.StartWait("Initializing helm client")
		defer log.StopWait()

		client, err := helmClient.NewClient(cmd.kubectl, false)
		if err != nil {
			log.Fatalf("Error initializing helm client: %s", err.Error())
		}

		cmd.helm = client
		log.Done("Initialized helm client")
	}
}

func (cmd *UpCmd) deployChart() {
	config := configutil.GetConfig(false)

	log.StartWait("Deploying helm chart")

	releaseName := *config.DevSpace.Release.Name
	releaseNamespace := *config.DevSpace.Release.Namespace
	chartPath := "chart/"

	values := map[interface{}]interface{}{}
	overwriteValues := map[interface{}]interface{}{}

	err := yamlutil.ReadYamlFromFile(chartPath+"values.yaml", values)
	if err != nil {
		log.Fatalf("Couldn't deploy chart, error reading from chart values %s: %v", chartPath+"values.yaml", err)
	}

	containerValues := map[string]interface{}{}

	for imageName, imageConf := range *config.Images {
		container := map[string]interface{}{}
		container["image"] = registry.GetImageURL(imageConf, true)

		if cmd.flags.noSleep {
			container["command"] = []string{}
			container["args"] = []string{}
		}

		containerValues[imageName] = container
	}

	pullSecrets := []interface{}{}
	existingPullSecrets, pullSecretsExisting := values["pullSecrets"]

	if pullSecretsExisting {
		pullSecrets = existingPullSecrets.([]interface{})
	}

	for _, registryConf := range *config.Registries {
		if registryConf.URL != nil {
			registrySecretName := registry.GetRegistryAuthSecretName(*registryConf.URL)
			pullSecrets = append(pullSecrets, registrySecretName)
		}
	}

	overwriteValues["containers"] = containerValues
	overwriteValues["pullSecrets"] = pullSecrets

	appRelease, err := cmd.helm.InstallChartByPath(releaseName, releaseNamespace, chartPath, &overwriteValues)

	log.StopWait()

	if err != nil {
		log.Fatalf("Unable to deploy helm chart: %s", err.Error())
	}

	releaseRevision := int(appRelease.Version)
	log.Donef("Deployed helm chart (Release revision: %d)", releaseRevision)
	log.StartWait("Waiting for release pod to become ready")
	defer log.StopWait()

	for true {
		podList, err := cmd.kubectl.Core().Pods(releaseNamespace).List(metav1.ListOptions{
			LabelSelector: "release=" + releaseName,
		})

		if err != nil {
			log.Panicf("Unable to list devspace pods: %s", err.Error())
		}

		if len(podList.Items) > 0 {
			highestRevision := 0
			var selectedPod k8sv1.Pod

			for i, pod := range podList.Items {
				podRevision, podHasRevision := pod.Annotations["revision"]
				hasHigherRevision := (i == 0)

				if !hasHigherRevision && podHasRevision {
					podRevisionInt, _ := strconv.Atoi(podRevision)

					if podRevisionInt > highestRevision {
						hasHigherRevision = true
					}
				}

				if hasHigherRevision {
					selectedPod = pod
					highestRevision, _ = strconv.Atoi(podRevision)
				}
			}
			_, hasRevision := selectedPod.Annotations["revision"]

			if !hasRevision || highestRevision == releaseRevision {
				if !hasRevision {
					log.Warn("Found pod without revision. Use annotation 'revision' for your pods to avoid this warning.")
				}

				cmd.pod = &selectedPod
				err = waitForPodReady(cmd.kubectl, cmd.pod, 2*60*time.Second, 5*time.Second)

				if err != nil {
					log.Fatalf("Error during waiting for pod: %s", err.Error())
				}

				break
			} else {
				log.Info("Waiting for release upgrade to complete.")
			}
		} else {
			log.Info("Waiting for release to be deployed.")
		}

		time.Sleep(2 * time.Second)
	}

	log.StopWait()
}

func (cmd *UpCmd) startSync() []*synctool.SyncConfig {
	config := configutil.GetConfig(false)
	syncConfigs := make([]*synctool.SyncConfig, 0, len(*config.DevSpace.Sync))

	for _, syncPath := range *config.DevSpace.Sync {
		absLocalPath, err := filepath.Abs(*syncPath.LocalSubPath)

		if err != nil {
			log.Panicf("Unable to resolve localSubPath %s: %s", *syncPath.LocalSubPath, err.Error())
		} else {
			// Retrieve pod from label selector
			labels := make([]string, 0, len(*syncPath.LabelSelector))

			for key, value := range *syncPath.LabelSelector {
				labels = append(labels, key+"="+*value)
			}

			namespace := *config.DevSpace.Release.Namespace
			if syncPath.Namespace != nil && *syncPath.Namespace != "" {
				namespace = *syncPath.Namespace
			}

			pod, err := kubectl.GetFirstRunningPod(cmd.kubectl, strings.Join(labels, ", "), namespace)

			if err != nil {
				log.Panicf("Unable to list devspace pods: %s", err.Error())
			} else if pod != nil {
				syncConfig := &synctool.SyncConfig{
					Kubectl:   cmd.kubectl,
					Pod:       pod,
					Container: &pod.Spec.Containers[0],
					WatchPath: absLocalPath,
					DestPath:  *syncPath.ContainerPath,
				}

				if syncPath.ExcludePaths != nil {
					syncConfig.ExcludePaths = *syncPath.ExcludePaths
				}

				if syncPath.DownloadExcludePaths != nil {
					syncConfig.DownloadExcludePaths = *syncPath.DownloadExcludePaths
				}

				if syncPath.UploadExcludePaths != nil {
					syncConfig.UploadExcludePaths = *syncPath.UploadExcludePaths
				}

				err = syncConfig.Start()
				if err != nil {
					log.Fatalf("Sync error: %s", err.Error())
				}

				log.Donef("Sync started on %s <-> %s", absLocalPath, *syncPath.ContainerPath)
				syncConfigs = append(syncConfigs, syncConfig)
			}
		}
	}

	return syncConfigs
}

func (cmd *UpCmd) startPortForwarding() {
	config := configutil.GetConfig(false)

	for _, portForwarding := range *config.DevSpace.PortForwarding {
		if *portForwarding.ResourceType == "pod" {
			if len(*portForwarding.LabelSelector) > 0 {
				labels := make([]string, 0, len(*portForwarding.LabelSelector))

				for key, value := range *portForwarding.LabelSelector {
					labels = append(labels, key+"="+*value)
				}

				namespace := *config.DevSpace.Release.Namespace
				if portForwarding.Namespace != nil && *portForwarding.Namespace != "" {
					namespace = *portForwarding.Namespace
				}

				pod, err := kubectl.GetFirstRunningPod(cmd.kubectl, strings.Join(labels, ", "), namespace)

				if err != nil {
					log.Errorf("Unable to list devspace pods: %s", err.Error())
				} else if pod != nil {
					ports := make([]string, len(*portForwarding.PortMappings))

					for index, value := range *portForwarding.PortMappings {
						ports[index] = strconv.Itoa(*value.LocalPort) + ":" + strconv.Itoa(*value.RemotePort)
					}

					readyChan := make(chan struct{})

					go kubectl.ForwardPorts(cmd.kubectl, pod, ports, make(chan struct{}), readyChan)

					// Wait till forwarding is ready
					select {
					case <-readyChan:
						log.Donef("Port forwarding started on %s", strings.Join(ports, ", "))
					case <-time.After(5 * time.Second):
						log.Error("Timeout waiting for port forwarding to start")
					}
				}
			}
		} else {
			log.Warn("Currently only pod resource type is supported for portforwarding")
		}
	}
}

func (cmd *UpCmd) enterTerminal() {
	var shell []string

	if len(cmd.flags.shell) == 0 {
		shell = []string{
			"sh",
			"-c",
			"command -v bash >/dev/null 2>&1 && exec bash || exec sh",
		}
	} else {
		shell = []string{cmd.flags.shell}
	}

	_, _, _, terminalErr := kubectl.Exec(cmd.kubectl, cmd.pod, cmd.pod.Spec.Containers[0].Name, shell, true, nil)

	if terminalErr != nil {
		if _, ok := terminalErr.(exec.CodeExitError); ok == false {
			log.Fatalf("Unable to start terminal session: %s", terminalErr.Error())
		}
	}
}

func waitForPodReady(kubectl *kubernetes.Clientset, pod *k8sv1.Pod, maxWaitTime time.Duration, checkInterval time.Duration) error {
	for maxWaitTime > 0 {
		pod, err := kubectl.Core().Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{})

		if err != nil {
			return err
		}

		if len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
			return nil
		}

		time.Sleep(checkInterval)
		maxWaitTime = maxWaitTime - checkInterval
	}

	return fmt.Errorf("Max wait time expired")
}
