package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"github.com/urfave/cli"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
)

var (
	VERSION = "0.0.1"

	FlagConfigFile            = "config"
	FlagProvisionerName       = "provisioner-name"
	EnvProvisionerName        = "PROVISIONER_NAME"
	DefaultProvisionerName    = "rancher.io/local-path"
	FlagNamespace             = "namespace"
	EnvNamespace              = "POD_NAMESPACE"
	DefaultNamespace          = "local-path-storage"
	FlagHelperImage           = "helper-image"
	EnvHelperImage            = "HELPER_IMAGE"
	DefaultHelperImage        = "busybox"
	FlagKubeconfig            = "kubeconfig"
	DefaultKubeConfigFilePath = ".kube/config"
	DefaultConfigFileKey      = "config.json"
	DefaultConfigMapName      = "local-path-config"
)

func cmdNotFound(c *cli.Context, command string) {
	panic(fmt.Errorf("Unrecognized command: %s", command))
}

func onUsageError(c *cli.Context, err error, isSubcommand bool) error {
	panic(fmt.Errorf("Usage error, please check your command"))
}

func RegisterShutdownChannel(done chan struct{}) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		logrus.Infof("Receive %v to exit", sig)
		close(done)
	}()
}

func StartCmd() cli.Command {
	return cli.Command{
		Name: "start",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  FlagConfigFile,
				Usage: "Required. Provisioner configuration file.",
				Value: "",
			},
			cli.StringFlag{
				Name:   FlagProvisionerName,
				Usage:  "Required. Specify Provisioner name.",
				EnvVar: EnvProvisionerName,
				Value:  DefaultProvisionerName,
			},
			cli.StringFlag{
				Name:   FlagNamespace,
				Usage:  "Required. The namespace that Provisioner is running in",
				EnvVar: EnvNamespace,
				Value:  DefaultNamespace,
			},
			cli.StringFlag{
				Name:   FlagHelperImage,
				Usage:  "Required. The helper image used for create/delete directories on the host",
				EnvVar: EnvHelperImage,
				Value:  DefaultHelperImage,
			},
			cli.StringFlag{
				Name:  FlagKubeconfig,
				Usage: "Paths to a kubeconfig. Only required when it is out-of-cluster.",
				Value: "",
			},
		},
		Action: func(c *cli.Context) {
			if err := startDaemon(c); err != nil {
				logrus.Fatalf("Error starting daemon: %v", err)
			}
		},
	}
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	home := homeDir()
	if kubeconfig == "" && home != "" {
		kubeconfig = filepath.Join(home, DefaultKubeConfigFilePath)
	}
	_, err := os.Stat(kubeconfig)
	if err != nil {
		return nil, err
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func findConfigFileFromConfigMap(kubeClient clientset.Interface, namespace string) (string, error) {
	cm, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(DefaultConfigMapName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	configFile, ok := cm.Data[DefaultConfigFileKey]
	if !ok {
		return "", fmt.Errorf("%v is not exist in local-path-config ConfigMap", DefaultConfigFileKey)
	}
	return configFile, nil
}

func startDaemon(c *cli.Context) error {
	stopCh := make(chan struct{})
	RegisterShutdownChannel(stopCh)

	config, err := loadConfig(c.String(FlagKubeconfig))
	if err != nil {
		return errors.Wrap(err, "unable to get client config")
	}

	kubeClient, err := clientset.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "unable to get k8s client")
	}

	serverVersion, err := kubeClient.Discovery().ServerVersion()
	if err != nil {
		return errors.Wrap(err, "Cannot start Provisioner: failed to get Kubernetes server version")
	}

	provisionerName := c.String(FlagProvisionerName)
	if provisionerName == "" {
		return fmt.Errorf("invalid empty flag %v", FlagProvisionerName)
	}
	namespace := c.String(FlagNamespace)
	if namespace == "" {
		return fmt.Errorf("invalid empty flag %v", FlagNamespace)
	}
	configFile := c.String(FlagConfigFile)
	if configFile == "" {
		configFile, err = findConfigFileFromConfigMap(kubeClient, namespace)
		if err != nil {
			return fmt.Errorf("invalid empty flag %v and it also does not exist at ConfigMap %v/%v", FlagConfigFile, namespace, DefaultConfigMapName)
		}
	}
	helperImage := c.String(FlagHelperImage)
	if helperImage == "" {
		return fmt.Errorf("invalid empty flag %v", FlagHelperImage)
	}

	provisioner, err := NewProvisioner(stopCh, kubeClient, configFile, namespace, helperImage)
	if err != nil {
		return err
	}
	pc := pvController.NewProvisionController(
		kubeClient,
		provisionerName,
		provisioner,
		serverVersion.GitVersion,
	)
	logrus.Debug("Provisioner started")
	pc.Run(stopCh)
	logrus.Debug("Provisioner stopped")
	return nil
}

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	a := cli.NewApp()
	a.Version = VERSION
	a.Usage = "Local Path Provisioner"

	a.Before = func(c *cli.Context) error {
		if c.GlobalBool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		return nil
	}

	a.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:   "debug, d",
			Usage:  "enable debug logging level",
			EnvVar: "RANCHER_DEBUG",
		},
	}
	a.Commands = []cli.Command{
		StartCmd(),
	}
	a.CommandNotFound = cmdNotFound
	a.OnUsageError = onUsageError

	if err := a.Run(os.Args); err != nil {
		logrus.Fatalf("Critical error: %v", err)
	}
}