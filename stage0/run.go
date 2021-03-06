// Copyright 2014 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//+build linux

package stage0

//
// rkt is a reference implementation of the app container specification.
//
// Execution on rkt is divided into a number of stages, and the `rkt`
// binary implements the first stage (stage 0)
//

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/coreos/rkt/common"
	"github.com/coreos/rkt/common/apps"
	"github.com/coreos/rkt/pkg/aci"
	"github.com/coreos/rkt/pkg/fileutil"
	"github.com/coreos/rkt/pkg/label"
	"github.com/coreos/rkt/pkg/sys"
	"github.com/coreos/rkt/pkg/uid"
	"github.com/coreos/rkt/store"
	"github.com/coreos/rkt/version"
)

const (
	// Default perm bits for the regular files
	// within the stage1 directory. (e.g. image manifest,
	// pod manifest, stage1ID, etc).
	defaultRegularFilePerm = os.FileMode(0640)

	// Default perm bits for the regular directories
	// within the stage1 directory.
	defaultRegularDirPerm = os.FileMode(0750)
)

var debugEnabled bool

// configuration parameters required by Prepare
type PrepareConfig struct {
	CommonConfig
	Apps         *apps.Apps          // apps to prepare
	InheritEnv   bool                // inherit parent environment into apps
	ExplicitEnv  []string            // always set these environment variables for all the apps
	Volumes      []types.Volume      // list of volumes that rkt can provide to applications
	Ports        []types.ExposedPort // list of ports that rkt will expose on the host
	UseOverlay   bool                // prepare pod with overlay fs
	PodManifest  string              // use the pod manifest specified by the user, this will ignore flags such as '--volume', '--port', etc.
	PrivateUsers *uid.UidRange       // User namespaces
}

// configuration parameters needed by Run
type RunConfig struct {
	CommonConfig
	Net         common.NetList // pod should have its own network stack
	LockFd      int            // lock file descriptor
	Interactive bool           // whether the pod is interactive or not
	MDSRegister bool           // whether to register with metadata service or not
	Apps        schema.AppList // applications (prepare gets them via Apps)
	LocalConfig string         // Path to local configuration
	RktGid      int            // group id of the 'rkt' group, -1 if there's no rkt group.
}

// configuration shared by both Run and Prepare
type CommonConfig struct {
	Store        *store.Store // store containing all of the configured application images
	Stage1Image  types.Hash   // stage1 image containing usable /init and /enter entrypoints
	UUID         *types.UUID  // UUID of the pod
	Debug        bool
	MountLabel   string // selinux label to use for fs
	ProcessLabel string // selinux label to use for process
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func InitDebug() {
	debugEnabled = true
}

func debug(format string, i ...interface{}) {
	if debugEnabled {
		log.Printf(format, i...)
	}
}

// MergeEnvs amends appEnv setting variables in setEnv before setting anything new from os.Environ if inheritEnv = true
// setEnv is expected to be in the os.Environ() key=value format
func MergeEnvs(appEnv *types.Environment, inheritEnv bool, setEnv []string) {
	for _, ev := range setEnv {
		pair := strings.SplitN(ev, "=", 2)
		appEnv.Set(pair[0], pair[1])
	}

	if inheritEnv {
		for _, ev := range os.Environ() {
			pair := strings.SplitN(ev, "=", 2)
			if _, exists := appEnv.Get(pair[0]); !exists {
				appEnv.Set(pair[0], pair[1])
			}
		}
	}
}

func imageNameToAppName(name types.ACIdentifier) (*types.ACName, error) {
	parts := strings.Split(name.String(), "/")
	last := parts[len(parts)-1]

	sn, err := types.SanitizeACName(last)
	if err != nil {
		return nil, err
	}

	return types.MustACName(sn), nil
}

// generatePodManifest creates the pod manifest from the command line input.
// It returns the pod manifest as []byte on success.
// This is invoked if no pod manifest is specified at the command line.
func generatePodManifest(cfg PrepareConfig, dir string) ([]byte, error) {
	pm := schema.PodManifest{
		ACKind: "PodManifest",
		Apps:   make(schema.AppList, 0),
	}

	v, err := types.NewSemVer(version.Version)
	if err != nil {
		return nil, fmt.Errorf("error creating version: %v", err)
	}
	pm.ACVersion = *v

	if err := cfg.Apps.Walk(func(app *apps.App) error {
		img := app.ImageID

		am, err := cfg.Store.GetImageManifest(img.String())
		if err != nil {
			return fmt.Errorf("error getting the manifest: %v", err)
		}
		appName, err := imageNameToAppName(am.Name)
		if err != nil {
			return fmt.Errorf("error converting image name to app name: %v", err)
		}
		if err := prepareAppImage(cfg, *appName, img, dir, cfg.UseOverlay); err != nil {
			return fmt.Errorf("error setting up image %s: %v", img, err)
		}
		if pm.Apps.Get(*appName) != nil {
			return fmt.Errorf("error: multiple apps with name %s", am.Name)
		}
		if am.App == nil && app.Exec == "" {
			return fmt.Errorf("error: image %s has no app section and --exec argument is not provided", img)
		}
		ra := schema.RuntimeApp{
			// TODO(vc): leverage RuntimeApp.Name for disambiguating the apps
			Name: *appName,
			App:  am.App,
			Image: schema.RuntimeImage{
				Name: &am.Name,
				ID:   img,
			},
			Annotations: am.Annotations,
		}

		if execOverride := app.Exec; execOverride != "" {
			// Create a minimal App section if not present
			if am.App == nil {
				ra.App = &types.App{
					User:  strconv.Itoa(os.Getuid()),
					Group: strconv.Itoa(os.Getgid()),
				}
			}
			ra.App.Exec = []string{execOverride}
		}

		if execAppends := app.Args; execAppends != nil {
			ra.App.Exec = append(ra.App.Exec, execAppends...)
		}

		if cfg.InheritEnv || len(cfg.ExplicitEnv) > 0 {
			MergeEnvs(&ra.App.Environment, cfg.InheritEnv, cfg.ExplicitEnv)
		}
		pm.Apps = append(pm.Apps, ra)
		return nil
	}); err != nil {
		return nil, err
	}

	// TODO(jonboulle): check that app mountpoint expectations are
	// satisfied here, rather than waiting for stage1
	pm.Volumes = cfg.Volumes
	pm.Ports = cfg.Ports

	pmb, err := json.Marshal(pm)
	if err != nil {
		return nil, fmt.Errorf("error marshalling pod manifest: %v", err)
	}
	return pmb, nil
}

// validatePodManifest reads the user-specified pod manifest, prepares the app images
// and validates the pod manifest. If the pod manifest passes validation, it returns
// the manifest as []byte.
// TODO(yifan): More validation in the future.
func validatePodManifest(cfg PrepareConfig, dir string) ([]byte, error) {
	pmb, err := ioutil.ReadFile(cfg.PodManifest)
	if err != nil {
		return nil, fmt.Errorf("error reading pod manifest: %v", err)
	}
	var pm schema.PodManifest
	if err := json.Unmarshal(pmb, &pm); err != nil {
		return nil, fmt.Errorf("error unmarshaling pod manifest: %v", err)
	}

	appNames := make(map[types.ACName]struct{})
	for _, ra := range pm.Apps {
		img := ra.Image

		if img.ID.Empty() {
			return nil, fmt.Errorf("no image ID for app %q", ra.Name)
		}
		am, err := cfg.Store.GetImageManifest(img.ID.String())
		if err != nil {
			return nil, fmt.Errorf("error getting the image manifest from store: %v", err)
		}
		if err := prepareAppImage(cfg, ra.Name, img.ID, dir, cfg.UseOverlay); err != nil {
			return nil, fmt.Errorf("error setting up image %s: %v", img, err)
		}
		if _, ok := appNames[ra.Name]; ok {
			return nil, fmt.Errorf("multiple apps with same name %s", ra.Name)
		}
		appNames[ra.Name] = struct{}{}
		if ra.App == nil && am.App == nil {
			return nil, fmt.Errorf("no app section in the pod manifest or the image manifest")
		}
	}
	return pmb, nil
}

// Prepare sets up a pod based on the given config.
func Prepare(cfg PrepareConfig, dir string, uuid *types.UUID) error {
	if err := os.MkdirAll(common.AppsInfoPath(dir), defaultRegularDirPerm); err != nil {
		return fmt.Errorf("error creating apps info directory: %v", err)
	}
	debug("Preparing stage1")
	if err := prepareStage1Image(cfg, cfg.Stage1Image, dir, cfg.UseOverlay); err != nil {
		return fmt.Errorf("error preparing stage1: %v", err)
	}

	var pmb []byte
	var err error
	if len(cfg.PodManifest) > 0 {
		pmb, err = validatePodManifest(cfg, dir)
	} else {
		pmb, err = generatePodManifest(cfg, dir)
	}
	if err != nil {
		return err
	}

	debug("Writing pod manifest")
	fn := common.PodManifestPath(dir)
	if err := ioutil.WriteFile(fn, pmb, defaultRegularFilePerm); err != nil {
		return fmt.Errorf("error writing pod manifest: %v", err)
	}

	if cfg.UseOverlay {
		// mark the pod as prepared with overlay
		f, err := os.Create(filepath.Join(dir, common.OverlayPreparedFilename))
		if err != nil {
			return fmt.Errorf("error writing overlay marker file: %v", err)
		}
		defer f.Close()
	}

	if cfg.PrivateUsers.Shift > 0 {
		// mark the pod as prepared for user namespaces
		uidrangeBytes := cfg.PrivateUsers.Serialize()

		if err := ioutil.WriteFile(filepath.Join(dir, common.PrivateUsersPreparedFilename), uidrangeBytes, defaultRegularFilePerm); err != nil {
			return fmt.Errorf("error writing userns marker file: %v", err)
		}
	}

	return nil
}

func preparedWithOverlay(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, common.OverlayPreparedFilename))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if !common.SupportsOverlay() {
		return false, fmt.Errorf("the pod was prepared with overlay but overlay is not supported")
	}

	return true, nil
}

func preparedWithPrivateUsers(dir string) (string, error) {
	bytes, err := ioutil.ReadFile(filepath.Join(dir, common.PrivateUsersPreparedFilename))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	return string(bytes), nil
}

// Run mounts the right overlay filesystems and actually runs the prepared
// pod by exec()ing the stage1 init inside the pod filesystem.
func Run(cfg RunConfig, dir string, dataDir string) {
	useOverlay, err := preparedWithOverlay(dir)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	privateUsers, err := preparedWithPrivateUsers(dir)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	debug("Setting up stage1")
	if err := setupStage1Image(cfg, dir, useOverlay); err != nil {
		log.Fatalf("error setting up stage1: %v", err)
	}
	debug("Wrote filesystem to %s\n", dir)

	for _, app := range cfg.Apps {
		if err := setupAppImage(cfg, app.Name, app.Image.ID, dir, useOverlay); err != nil {
			log.Fatalf("error setting up app image: %v", err)
		}
	}

	destRootfs := common.Stage1RootfsPath(dir)
	flavor, err := os.Readlink(filepath.Join(destRootfs, "flavor"))
	if err != nil {
		log.Printf("error reading flavor: %v\n", err)
	}
	if flavor == "kvm" {
		err := kvmCheckSSHSetup(destRootfs, dataDir)
		if err != nil {
			log.Fatalf("error setting up ssh keys: %v", err)
		}
	}

	if err := os.Setenv(common.EnvLockFd, fmt.Sprintf("%v", cfg.LockFd)); err != nil {
		log.Fatalf("setting lock fd environment: %v", err)
	}

	if err := os.Setenv(common.SELinuxContext, fmt.Sprintf("%v", cfg.ProcessLabel)); err != nil {
		log.Fatalf("setting SELinux context environment: %v", err)
	}

	debug("Pivoting to filesystem %s", dir)
	if err := os.Chdir(dir); err != nil {
		log.Fatalf("failed changing to dir: %v", err)
	}

	ep, err := getStage1Entrypoint(dir, runEntrypoint)
	if err != nil {
		log.Fatalf("error determining init entrypoint: %v", err)
	}
	args := []string{filepath.Join(destRootfs, ep)}
	debug("Execing %s", ep)

	if cfg.Debug {
		args = append(args, "--debug")
	}

	args = append(args, "--net="+cfg.Net.String())

	if cfg.Interactive {
		args = append(args, "--interactive")
	}
	if len(privateUsers) > 0 {
		args = append(args, "--private-users="+privateUsers)
	}
	if cfg.MDSRegister {
		mdsToken, err := registerPod(".", cfg.UUID, cfg.Apps)
		if err != nil {
			log.Fatalf("failed to register the pod: %v", err)
		}

		args = append(args, "--mds-token="+mdsToken)
	}

	if cfg.LocalConfig != "" {
		args = append(args, "--local-config="+cfg.LocalConfig)
	}

	args = append(args, cfg.UUID.String())

	// make sure the lock fd stays open across exec
	if err := sys.CloseOnExec(cfg.LockFd, false); err != nil {
		log.Fatalf("error clearing FD_CLOEXEC on lock fd")
	}

	if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
		log.Fatalf("error execing init: %v", err)
	}
}

// prepareAppImage renders and verifies the tree cache of the app image that
// corresponds to the given app name.
// When useOverlay is false, it attempts to render and expand the app image
func prepareAppImage(cfg PrepareConfig, appName types.ACName, img types.Hash, cdir string, useOverlay bool) error {
	debug("Loading image %s", img.String())

	am, err := cfg.Store.GetImageManifest(img.String())
	if err != nil {
		return fmt.Errorf("error getting the manifest: %v", err)
	}

	if _, hasOS := am.Labels.Get("os"); !hasOS {
		return fmt.Errorf("missing os label in the image manifest")
	}
	if _, hasArch := am.Labels.Get("arch"); !hasArch {
		return fmt.Errorf("missing arch label in the image manifest")
	}

	if err := types.IsValidOSArch(am.Labels.ToMap(), ValidOSArch); err != nil {
		return err
	}

	appInfoDir := common.AppInfoPath(cdir, appName)
	if err := os.MkdirAll(appInfoDir, defaultRegularDirPerm); err != nil {
		return fmt.Errorf("error creating apps info directory: %v", err)
	}

	if useOverlay {
		if cfg.PrivateUsers.Shift > 0 {
			return fmt.Errorf("cannot use both overlay and user namespace: not implemented yet. (Try --no-overlay)")
		}
		treeStoreID, err := cfg.Store.RenderTreeStore(img.String(), false)
		if err != nil {
			return fmt.Errorf("error rendering tree image: %v", err)
		}
		if err := cfg.Store.CheckTreeStore(treeStoreID); err != nil {
			log.Printf("Warning: tree cache is in a bad state: %v. Rebuilding...", err)
			var err error
			if treeStoreID, err = cfg.Store.RenderTreeStore(img.String(), true); err != nil {
				return fmt.Errorf("error rendering tree image: %v", err)
			}
		}

		if err := ioutil.WriteFile(common.AppTreeStoreIDPath(cdir, appName), []byte(treeStoreID), defaultRegularFilePerm); err != nil {
			return fmt.Errorf("error writing app treeStoreID: %v", err)
		}
	} else {
		ad := common.AppPath(cdir, appName)
		err := os.MkdirAll(ad, defaultRegularDirPerm)
		if err != nil {
			return fmt.Errorf("error creating image directory: %v", err)
		}

		shiftedUid, shiftedGid, err := cfg.PrivateUsers.ShiftRange(uint32(os.Getuid()), uint32(os.Getgid()))
		if err != nil {
			return fmt.Errorf("error getting uid, gid: %v", err)
		}

		if err := os.Chown(ad, int(shiftedUid), int(shiftedGid)); err != nil {
			return fmt.Errorf("error shifting app %q's stage2 dir: %v", appName, err)
		}

		if err := aci.RenderACIWithImageID(img, ad, cfg.Store, cfg.PrivateUsers); err != nil {
			return fmt.Errorf("error rendering ACI: %v", err)
		}
	}
	if err := writeManifest(cfg.CommonConfig, img, appInfoDir); err != nil {
		return err
	}
	return nil
}

// setupAppImage mounts the overlay filesystem for the app image that
// corresponds to the given hash. Then, it creates the tmp directory.
// When useOverlay is false it just creates the tmp directory for this app.
func setupAppImage(cfg RunConfig, appName types.ACName, img types.Hash, cdir string, useOverlay bool) error {
	ad := common.AppPath(cdir, appName)
	if useOverlay {
		err := os.MkdirAll(ad, defaultRegularDirPerm)
		if err != nil {
			return fmt.Errorf("error creating image directory: %v", err)
		}
		treeStoreID, err := ioutil.ReadFile(common.AppTreeStoreIDPath(cdir, appName))
		if err != nil {
			return err
		}
		if err := copyAppManifest(cdir, appName, ad); err != nil {
			return err
		}
		if err := overlayRender(cfg, string(treeStoreID), cdir, ad, appName.String()); err != nil {
			return fmt.Errorf("error rendering overlay filesystem: %v", err)
		}
	}

	return nil
}

// prepareStage1Image renders and verifies tree cache of the given hash
// when using overlay.
// When useOverlay is false, it attempts to render and expand the stage1.
func prepareStage1Image(cfg PrepareConfig, img types.Hash, cdir string, useOverlay bool) error {
	s1 := common.Stage1ImagePath(cdir)
	if err := os.MkdirAll(s1, defaultRegularDirPerm); err != nil {
		return fmt.Errorf("error creating stage1 directory: %v", err)
	}

	treeStoreID, err := cfg.Store.RenderTreeStore(img.String(), false)
	if err != nil {
		return fmt.Errorf("error rendering tree image: %v", err)
	}
	if err := cfg.Store.CheckTreeStore(treeStoreID); err != nil {
		log.Printf("Warning: tree cache is in a bad state: %v. Rebuilding...", err)
		var err error
		if treeStoreID, err = cfg.Store.RenderTreeStore(img.String(), true); err != nil {
			return fmt.Errorf("error rendering tree image: %v", err)
		}
	}

	if err := writeManifest(cfg.CommonConfig, img, s1); err != nil {
		return fmt.Errorf("error writing manifest: %v", err)
	}

	if !useOverlay {
		destRootfs := filepath.Join(s1, "rootfs")
		cachedTreePath := cfg.Store.GetTreeStoreRootFS(treeStoreID)
		if err := fileutil.CopyTree(cachedTreePath, destRootfs, cfg.PrivateUsers); err != nil {
			return fmt.Errorf("error rendering ACI: %v", err)
		}
	}

	fn := path.Join(cdir, common.Stage1TreeStoreIDFilename)
	if err := ioutil.WriteFile(fn, []byte(treeStoreID), defaultRegularFilePerm); err != nil {
		return fmt.Errorf("error writing stage1 treeStoreID: %v", err)
	}
	return nil
}

// setupStage1Image mounts the overlay filesystem for stage1.
// When useOverlay is false it is a noop
func setupStage1Image(cfg RunConfig, cdir string, useOverlay bool) error {
	s1 := common.Stage1ImagePath(cdir)
	if useOverlay {
		treeStoreID, err := ioutil.ReadFile(filepath.Join(cdir, common.Stage1TreeStoreIDFilename))
		if err != nil {
			return err
		}

		// pass an empty appName: make sure it remains consistent with
		// overlayStatusDirTemplate
		if err := overlayRender(cfg, string(treeStoreID), cdir, s1, ""); err != nil {
			return fmt.Errorf("error rendering overlay filesystem: %v", err)
		}

		// we will later read the status from the upper layer of the overlay fs
		// force the status directory to be there by touching it
		statusPath := filepath.Join(s1, "rootfs", "rkt", "status")
		if err := os.Chtimes(statusPath, time.Now(), time.Now()); err != nil {
			return fmt.Errorf("error touching status dir: %v", err)
		}
	}

	// Chown the 'rootfs' directory, so that rkt list/rkt status can access it.
	if err := os.Chown(filepath.Join(s1, "rootfs"), -1, cfg.RktGid); err != nil {
		return err
	}

	// Chown the 'rootfs/rkt' and its sub-directory, so that rkt list/rkt status can
	// access it.
	// Also set 'S_ISGID' bit on the mode so that when the app writes exit status to
	// 'rootfs/rkt/status/$app', the file has the group ID set to 'rkt'.
	chownWalker := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := os.Chown(path, -1, cfg.RktGid); err != nil {
			return err
		}
		if err := os.Chmod(path, info.Mode()|os.ModeSetgid); err != nil {
			return err
		}
		return nil
	}

	return filepath.Walk(filepath.Join(s1, "rootfs", "rkt"), chownWalker)
}

// writeManifest takes an img ID and writes the corresponding manifest in dest
func writeManifest(cfg CommonConfig, img types.Hash, dest string) error {
	manifest, err := cfg.Store.GetImageManifest(img.String())
	if err != nil {
		return err
	}

	mb, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("error marshalling image manifest: %v", err)
	}

	debug("Writing image manifest")
	if err := ioutil.WriteFile(filepath.Join(dest, "manifest"), mb, defaultRegularFilePerm); err != nil {
		return fmt.Errorf("error writing image manifest: %v", err)
	}

	return nil
}

// copyAppManifest copies to saved image manifest for the given appName and
// writes it in the dest directory.
func copyAppManifest(cdir string, appName types.ACName, dest string) error {
	appInfoDir := common.AppInfoPath(cdir, appName)
	sourceFn := filepath.Join(appInfoDir, "manifest")
	destFn := filepath.Join(dest, "manifest")
	if err := fileutil.CopyRegularFile(sourceFn, destFn); err != nil {
		return fmt.Errorf("error copying image manifest: %v", err)
	}
	return nil
}

// overlayRender renders the image that corresponds to the given hash using the
// overlay filesystem.
// It mounts an overlay filesystem from the cached tree of the image as rootfs.
func overlayRender(cfg RunConfig, treeStoreID string, cdir string, dest string, appName string) error {
	destRootfs := path.Join(dest, "rootfs")
	if err := os.MkdirAll(destRootfs, defaultRegularDirPerm); err != nil {
		return err
	}

	cachedTreePath := cfg.Store.GetTreeStoreRootFS(treeStoreID)

	overlayDir := path.Join(cdir, "overlay")
	if err := os.MkdirAll(overlayDir, defaultRegularDirPerm); err != nil {
		return err
	}

	// Since the parent directory (rkt/pods/$STATE/$POD_UUID) has the 'S_ISGID' bit, here
	// we need to explicitly turn the bit off when creating this overlay
	// directory so that it won't inherit the bit. Otherwise the files
	// created by users within the pod will inherit the 'S_ISGID' bit
	// as well.
	if err := os.Chmod(overlayDir, defaultRegularDirPerm); err != nil {
		return err
	}

	imgDir := path.Join(overlayDir, treeStoreID)
	if err := os.MkdirAll(imgDir, defaultRegularDirPerm); err != nil {
		return err
	}

	// Also make 'rkt/pods/$STATE/$POD_UUID/overlay/$IMAGE_ID' to be readable by 'rkt' group
	// As 'rkt' status will read the 'rkt/pods/$STATE/$POD_UUID/overlay/$IMAGE_ID/upper/rkt/status/$APP'
	// to get exit status.
	if err := os.Chown(imgDir, -1, cfg.RktGid); err != nil {
		return err
	}

	upperDir := path.Join(imgDir, "upper", appName)
	if err := os.MkdirAll(upperDir, defaultRegularDirPerm); err != nil {
		return err
	}
	if err := label.SetFileLabel(upperDir, cfg.MountLabel); err != nil {
		return err
	}

	workDir := path.Join(imgDir, "work", appName)
	if err := os.MkdirAll(workDir, defaultRegularDirPerm); err != nil {
		return err
	}
	if err := label.SetFileLabel(workDir, cfg.MountLabel); err != nil {
		return err
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", cachedTreePath, upperDir, workDir)
	opts = label.FormatMountLabel(opts, cfg.MountLabel)
	if err := syscall.Mount("overlay", destRootfs, "overlay", 0, opts); err != nil {
		return fmt.Errorf("error mounting: %v", err)
	}

	return nil
}
