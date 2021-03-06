package occlum

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	shim_config "github.com/alibaba/inclavare-containers/shim/config"
	"github.com/alibaba/inclavare-containers/shim/runtime/carrier"
	carr_const "github.com/alibaba/inclavare-containers/shim/runtime/carrier/constants"
	"github.com/alibaba/inclavare-containers/shim/runtime/config"
	"github.com/alibaba/inclavare-containers/shim/runtime/utils"
	"github.com/alibaba/inclavare-containers/shim/runtime/v2/rune/constants"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/runtime/v2/task"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

const (
	defaultNamespace         = "k8s.io"
	replaceOcclumImageScript = "replace_occlum_image.sh"
	carrierScriptFileName    = "carrier.sh"
	startScriptFileName      = "start.sh"
	rootfsDirName            = "rootfs"
	enclaveDataDir           = "data"
)

var _ carrier.Carrier = &occlum{}

type occlumBuildTask struct {
	client    *containerd.Client
	container *containerd.Container
	task      *containerd.Task
}

type occlum struct {
	context       context.Context
	bundle        string
	workDirectory string
	entryPoints   []string
	configPath    string
	task          *occlumBuildTask
	spec          *specs.Spec
	shimConfig    *shim_config.Config
}

// NewOcclumCarrier returns an carrier instance of occlum.
func NewOcclumCarrier(ctx context.Context, bundle string) (carrier.Carrier, error) {
	var cfg shim_config.Config
	if _, err := toml.DecodeFile(constants.ConfigurationPath, &cfg); err != nil {
		return nil, err
	}

	setLogLevel(cfg.LogLevel)

	return &occlum{
		context:    ctx,
		bundle:     bundle,
		shimConfig: &cfg,
		task:       &occlumBuildTask{},
	}, nil
}

// Name impl Carrier.
func (c *occlum) Name() string {
	return "occlum"
}

// BuildUnsignedEnclave impl Carrier.
func (c *occlum) BuildUnsignedEnclave(req *task.CreateTaskRequest, args *carrier.BuildUnsignedEnclaveArgs) (
	unsignedEnclave string, err error) {

	// Initialize environment variables for occlum in config.json
	if err := c.initBundleConfig(); err != nil {
		return "", err
	}

	namespace, ok := namespaces.Namespace(c.context)
	if !ok {
		namespace = defaultNamespace
	}
	// Create a new client connected to the default socket path for containerd.
	client, err := containerd.New(c.shimConfig.Containerd.Socket)
	if err != nil {
		return "", fmt.Errorf("failed to create containerd client. error: %++v", err)
	} else {
		c.task.client = client
	}
	logrus.Debugf("BuildUnsignedEnclave: get containerd client successfully")

	if err = createNamespaceIfNotExist(client, namespace); err != nil {
		logrus.Errorf("BuildUnsignedEnclave: create namespace %s failed. error: %++v", namespace, err)
		return "", err
	}

	// pull the image that used to build enclave.
	occlumEnclaveBuilderImage := c.shimConfig.EnclaveRuntime.Occlum.BuildImage
	image, err := client.Pull(c.context, occlumEnclaveBuilderImage, containerd.WithPullUnpack)
	if err != nil {
		return "", fmt.Errorf("failed to pull image %s. error: %++v", occlumEnclaveBuilderImage, err)
	}
	logrus.Debugf("BuildUnsignedEnclave: pull image %s successfully", occlumEnclaveBuilderImage)

	// Generate the containerId and snapshotId.
	// FIXME debug
	rand.Seed(time.Now().UnixNano())
	containerId := fmt.Sprintf("occlum-enclave-builder-%s", strconv.FormatInt(rand.Int63(), 16))
	snapshotId := fmt.Sprintf("occlum-enclave-builder-snapshot-%s", strconv.FormatInt(rand.Int63(), 16))

	logrus.Debugf("BuildUnsignedEnclave: containerId: %s, snapshotId: %s", containerId, snapshotId)

	if err := os.Mkdir(filepath.Join(req.Bundle, enclaveDataDir), 0755); err != nil {
		return "", err
	}

	replaceImagesScript := filepath.Join(req.Bundle, enclaveDataDir, replaceOcclumImageScript)
	if err := ioutil.WriteFile(replaceImagesScript, []byte(carr_const.ReplaceOcclumImageScript), os.ModePerm); err != nil {
		return "", err
	}

	carrierScript := filepath.Join(req.Bundle, enclaveDataDir, carrierScriptFileName)
	if err := ioutil.WriteFile(carrierScript, []byte(carr_const.CarrierScript), os.ModePerm); err != nil {
		return "", err
	}

	startScript := filepath.Join(req.Bundle, enclaveDataDir, startScriptFileName)
	if err := ioutil.WriteFile(startScript, []byte(carr_const.StartScript), os.ModePerm); err != nil {
		return "", err
	}

	// Create rootfs mount points.
	mounts := make([]specs.Mount, 0)
	rootfsMount := specs.Mount{
		Destination: filepath.Join("/", rootfsDirName),
		Type:        "bind",
		Source:      filepath.Join(req.Bundle, rootfsDirName),
		Options:     []string{"rbind", "rw"},
	}
	dataMount := specs.Mount{
		Destination: filepath.Join("/", enclaveDataDir),
		Type:        "bind",
		Source:      filepath.Join(req.Bundle, enclaveDataDir),
		Options:     []string{"rbind", "rw"},
	}

	logrus.Debugf("BuildUnsignedEnclave: rootfsMount source: %s, destination: %s",
		rootfsMount.Source, rootfsMount.Destination)

	mounts = append(mounts, rootfsMount, dataMount)
	// create a container
	container, err := client.NewContainer(
		c.context,
		containerId,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(snapshotId, image),
		containerd.WithNewSpec(oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/bash", filepath.Join("/", enclaveDataDir, startScriptFileName)),
			oci.WithPrivileged,
			oci.WithMounts(mounts),
		),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create container by image %s. error: %++v",
			occlumEnclaveBuilderImage, err)
	} else {
		c.task.container = &container
	}

	// Create a task from the container.
	t, err := container.NewTask(c.context, cio.NewCreator(cio.WithStdio))
	if err != nil {
		return "", err
	} else {
		c.task.task = &t
	}
	logrus.Debugf("BuildUnsignedEnclave: create task successfully")

	if err := t.Start(c.context); err != nil {
		logrus.Errorf("BuildUnsignedEnclave: start task failed. error: %++v", err)
		return "", err
	}

	cmd := []string{
		"/bin/bash", filepath.Join("/", enclaveDataDir, carrierScriptFileName),
		"--action", "buildUnsignedEnclave",
		"--entry_point", c.entryPoints[0],
		"--work_dir", c.workDirectory,
		"--rootfs", filepath.Join("/", rootfsDirName),
	}
	if c.configPath != "" {
		cmd = append(cmd, "--occlum_config_path", filepath.Join("/", rootfsDirName, c.configPath))
	}
	logrus.Debugf("BuildUnsignedEnclave: command: %v", cmd)
	if err := c.execTask(cmd...); err != nil {
		logrus.Errorf("BuildUnsignedEnclave: exec failed. error: %++v", err)
		return "", err
	}
	enclavePath := filepath.Join("/", rootfsDirName, c.workDirectory, ".occlum/build/lib/libocclum-libos.so")

	return enclavePath, nil
}

// GenerateSigningMaterial impl Carrier.
func (c *occlum) GenerateSigningMaterial(req *task.CreateTaskRequest, args *carrier.CommonArgs) (
	signingMaterial string, err error) {
	signingMaterial = filepath.Join("/", rootfsDirName, c.workDirectory, "enclave_sig.dat")
	args.Config = filepath.Join("/", rootfsDirName, c.workDirectory, "Enclave.xml")
	cmd := []string{
		"/bin/bash", filepath.Join("/", enclaveDataDir, carrierScriptFileName),
		"--action", "generateSigningMaterial",
		"--enclave_config_path", args.Config,
		"--unsigned_encalve_path", args.Enclave,
		"--unsigned_material_path", signingMaterial,
	}
	logrus.Debugf("GenerateSigningMaterial: sgx_sign gendata command: %v", cmd)
	if err := c.execTask(cmd...); err != nil {
		logrus.Errorf("GenerateSigningMaterial: sgx_sign gendata failed. error: %++v", err)
		return "", err
	}
	logrus.Debugf("GenerateSigningMaterial: sgx_sign gendata successfully")
	return signingMaterial, nil
}

// CascadeEnclaveSignature impl Carrier.
func (c *occlum) CascadeEnclaveSignature(req *task.CreateTaskRequest, args *carrier.CascadeEnclaveSignatureArgs) (
	signedEnclave string, err error) {
	var bufferSize int64 = 1024 * 4
	signedEnclave = filepath.Join("/", rootfsDirName, c.workDirectory, ".occlum/build/lib/libocclum-libos.signed.so")
	publicKey := filepath.Join("/", enclaveDataDir, "public_key.pem")
	signature := filepath.Join("/", enclaveDataDir, "signature.dat")
	if err := utils.CopyFile(args.Key, filepath.Join(req.Bundle, publicKey), bufferSize); err != nil {
		logrus.Errorf("CascadeEnclaveSignature copy file %s to %s failed. err: %++v", args.Key, publicKey, err)
		return "", err
	}
	if err := utils.CopyFile(args.Signature, filepath.Join(req.Bundle, signature), bufferSize); err != nil {
		logrus.Errorf("CascadeEnclaveSignature copy file %s to %s failed. err: %++v", args.Signature, signature, err)
		return "", err
	}
	cmd := []string{
		"/bin/bash", filepath.Join("/", enclaveDataDir, carrierScriptFileName),
		"--action", "cascadeEnclaveSignature",
		"--enclave_config_path", args.Config,
		"--unsigned_encalve_path", args.Enclave,
		"--unsigned_material_path", args.SigningMaterial,
		"--signed_enclave_path", signedEnclave,
		"--public_key_path", publicKey,
		"--signature_path", signature,
	}
	logrus.Debugf("CascadeEnclaveSignature: sgx_sign catsig command: %v", cmd)
	if err := c.execTask(cmd...); err != nil {
		logrus.Errorf("CascadeEnclaveSignature: sgx_sign catsig failed. error: %++v", err)
		return "", err
	}
	logrus.Debugf("CascadeEnclaveSignature: sgx_sign catsig successfully")
	return signedEnclave, nil
}

// Cleanup impl Carrier.
func (c *occlum) Cleanup() error {
	defer func() {
		if c.task.client != nil {
			c.task.client.Close()
		}
	}()
	defer func() {
		if c.task.container != nil {
			container := *c.task.container
			if err := container.Delete(c.context, containerd.WithSnapshotCleanup); err != nil {
				logrus.Errorf("Cleanup: delete container %s failed. err: %++v", container.ID(), err)
			}
			logrus.Debugf("Cleanup: delete container %s successfully.", container.ID())
		}
	}()

	if c.task.task == nil {
		return nil
	}
	t := *c.task.task
	if err := t.Kill(c.context, syscall.SIGTERM); err != nil {
		logrus.Errorf("Cleanup: kill task %s failed. err: %++v", t.ID(), err)
		return err
	}
	for {
		status, err := t.Status(c.context)
		if err != nil {
			logrus.Errorf("Cleanup: get task %s status failed. error: %++v", t.ID(), err)
			return err
		}
		if status.ExitStatus != 0 {
			logrus.Errorf("Cleanup: task %s exit abnormally. exit code: %d, task status: %s", t.ID(),
				status.ExitStatus, status.Status)
			return fmt.Errorf("task  %s exit abnormally. exit code: %d, task status: %s",
				t.ID(), status.ExitStatus, status.Status)
		}
		if status.Status != containerd.Stopped {
			logrus.Debugf("Cleanup: task %s status: %s", t.ID(), status.Status)
			time.Sleep(time.Second)
			continue
		}
		break
	}
	if _, err := t.Delete(c.context); err != nil {
		logrus.Errorf("Cleanup: delete task %s failed. error: %++v", t.ID(), err)
		return err
	}
	logrus.Debugf("Cleanup: clean occlum container and task successfully")
	return nil
}

func (c *occlum) initBundleConfig() error {
	configPath := filepath.Join(c.bundle, "config.json")
	spec, err := config.LoadSpec(configPath)
	if err != nil {
		return err
	}
	c.workDirectory = spec.Process.Cwd
	c.entryPoints = spec.Process.Args
	enclaveRuntimePath := fmt.Sprintf("%s/liberpal-occlum.so", c.workDirectory)
	envs := map[string]string{
		carr_const.EnclaveRuntimePathKeyName: enclaveRuntimePath,
		carr_const.EnclaveTypeKeyName:        string(carr_const.IntelSGX),
		carr_const.EnclaveRuntimeArgsKeyName: carr_const.DefaultEnclaveRuntimeArgs,
	}
	occlumConfigPath, ok := config.GetEnv(spec, carr_const.OcclumConfigPathKeyName)
	if ok {
		c.configPath = occlumConfigPath
	}
	c.spec = spec
	if err := config.UpdateEnvs(spec, envs, false); err != nil {
		return err
	}
	return config.SaveSpec(configPath, spec)
}

func createNamespaceIfNotExist(client *containerd.Client, namespace string) error {
	svc := client.NamespaceService()

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()
	nses, err := svc.List(ctx)
	if err != nil {
		return err
	}
	for _, ns := range nses {
		if ns == namespace {
			return nil
		}
	}

	return svc.Create(ctx, namespace, nil)
}

func setLogLevel(level string) {
	switch level {
	case "debug":
		logrus.SetLevel(logrus.DebugLevel)
	case "info":
		logrus.SetLevel(logrus.InfoLevel)
	case "warn":
		logrus.SetLevel(logrus.WarnLevel)
	case "error":
		logrus.SetLevel(logrus.ErrorLevel)
	default:
		logrus.SetLevel(logrus.InfoLevel)
	}
}

func (c *occlum) execTask(args ...string) error {
	container := *c.task.container
	t := *c.task.task
	if container == nil || t == nil {
		return fmt.Errorf("task is not exist")
	}
	spec, err := container.Spec(c.context)
	if err != nil {
		logrus.Errorf("execTask: get container spec failed. error: %++v", err)
		return err
	}
	pspec := spec.Process
	pspec.Terminal = false
	pspec.Args = args

	cioOpts := []cio.Opt{cio.WithStdio, cio.WithFIFODir("/run/containerd/fifo")}
	ioCreator := cio.NewCreator(cioOpts...)
	process, err := t.Exec(c.context, utils.GenerateID(), pspec, ioCreator)
	if err != nil {
		logrus.Errorf("execTask: exec process in task failed. error: %++v", err)
		return err
	}
	defer process.Delete(c.context)
	statusC, err := process.Wait(c.context)
	if err != nil {
		return err
	}
	sigc := commands.ForwardAllSignals(c.context, process)
	defer commands.StopCatch(sigc)

	if err := process.Start(c.context); err != nil {
		logrus.Errorf("execTask: start process failed. error: %++v", err)
		return err
	}
	status := <-statusC
	code, _, err := status.Result()
	if err != nil {
		logrus.Errorf("execTask: exec process failed. error: %++v", err)
		return err
	}
	if code != 0 {
		return fmt.Errorf("process exit abnormaly. exitCode: %d, error: %++v", code, status.Error())
	}
	logrus.Debugf("execTask: exec successfully.")
	return nil
}
