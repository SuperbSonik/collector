package main

// Collector is a program that extracts static information from container images stored in a Docker registry.

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	blog "github.com/ccpaging/log4go"
	flag "github.com/docker/docker/pkg/mflag"
)

const (
	// Console logging level
	CONSOLELOGLEVEL = blog.INFO
	// File logging level
	FILELOGLEVEL = blog.FINEST
	// Time to wait before retrying a failed operation.
	RETRYDURATION = time.Duration(5) * time.Second
	// Number of docker images to process in a single batch.
	IMAGEBATCH = 5
)

var (
	// Directories/files dependent on Environment variables
	BANYANHOSTDIR = func() string {
		if os.Getenv("BANYAN_HOST_DIR") == "" {
			os.Setenv("BANYAN_HOST_DIR", os.Getenv("HOME")+"/.banyan")
		}
		return os.Getenv("BANYAN_HOST_DIR")
	}
	BANYANDIR = func() string {
		if os.Getenv("BANYAN_DIR") != "" {
			return os.Getenv("BANYAN_DIR")
		}
		return BANYANHOSTDIR()
	}
	COLLECTORDIR = func() string {
		if os.Getenv("COLLECTOR_DIR") == "" {
			fmt.Fprintf(os.Stderr, "Please set the environment variable COLLECTOR_DIR to the parent")
			fmt.Fprintf(os.Stderr, " of the \"data\" scripts directory...\n\n")
			//printExampleUsage()
			//fmt.Fprintf(os.Stderr, "  e.g.,\tcd COLLECTOR_SOURCE_DIRECTORY\n")
			//fmt.Fprintf(os.Stderr, "\tsudo COLLECTOR_DIR=$PWD collector [options] REGISTRY [REPO1 REPO2 ...]\n\n")
			return ""
		}
		return os.Getenv("COLLECTOR_DIR")
	}
	LOGFILENAME  = BANYANDIR() + "/hostcollector/collector.log"
	banyanOutDir = flag.String([]string{"#-banyanoutdir"}, BANYANDIR()+"/hostcollector/banyanout",
		"Output directory for collected data")
	imageList = flag.String([]string{"#-imagelist"}, BANYANDIR()+"/hostcollector/imagelist",
		"List of previously collected images (file)")
	repoList = flag.String([]string{"r", "-repolist"}, BANYANDIR()+"/hostcollector/repolist",
		"File containing list of repos to process")

	// Configuration parameters for speed/efficiency
	removeThresh = flag.Int([]string{"-removethresh"}, 10,
		"Number of images that get pulled before removal")
	poll = flag.Int64([]string{"p", "-poll"}, 60, "Polling interval in seconds")

	// Docker remote API related parameters
	dockerProto = flag.String([]string{"-dockerproto"}, "unix",
		"Socket protocol for Docker Remote API (\"unix\" or \"tcp\")")
	dockerAddr = flag.String([]string{"-dockeraddr"}, "/var/run/docker.sock",
		"Address of Docker remote API socket (filepath or IP:port)")

	// Output options
	dests = flag.String([]string{"d", "-dests"}, "file",
		"One or more ',' separated destinations for output generated by scripts. e.g., file or file,banyan or banyan")
	writerList []Writer

	// positional arguments: a list of repos to process, all others are ignored.
	reposToProcess = make(map[RepoType]bool)
	filterRepos    = false
	excludeRepo    = func() map[RepoType]bool {
		excludeList := []RepoType{} // You can add repos to this list
		m := make(map[RepoType]bool)
		for _, r := range excludeList {
			m[r] = true
		}
		return m
	}()
)

// ImageSet is a set of image IDs.
type ImageSet map[ImageIDType]bool

// NewImageSet creates a new ImageSet.
func NewImageSet() ImageSet {
	return ImageSet(make(map[ImageIDType]bool))
}

func getImageToMDMap(imageMDs []ImageMetadataInfo) (imageToMDMap map[string][]ImageMetadataInfo) {
	imageToMDMap = make(map[string][]ImageMetadataInfo)
	for _, imageMD := range imageMDs {
		imageToMDMap[imageMD.Image] = append(imageToMDMap[imageMD.Image], imageMD)
	}
	return
}

// DoIteration runs one iteration of the main loop to get new images, extract packages and dependencies,
// and save results.
func DoIteration(authToken string, processedImages ImageSet, oldImiSet ImiSet,
	PulledList []ImageMetadataInfo) (currentImiSet ImiSet, PulledNew []ImageMetadataInfo) {
	blog.Debug("DoIteration: processedImages is %v", processedImages)
	PulledNew = PulledList
	_ /*tagSlice*/, imi, currentImiSet := getNewImageMetadata(oldImiSet)
	imageToMDMap := getImageToMDMap(imi)
	if len(imi) > 0 {
		SaveImageMetadata(imi)

		for {
			pulledImages := NewImageSet()
			for _, metadata := range imi {
				if filterRepos && !reposToProcess[RepoType(metadata.Repo)] {
					continue
				}
				if excludeRepo[RepoType(metadata.Repo)] {
					continue
				}
				if pulledImages[ImageIDType(metadata.Image)] {
					continue
				}
				if processedImages[ImageIDType(metadata.Image)] {
					continue
				}
				// docker pull image
				PullImage(metadata)
				PulledNew = append(PulledNew, metadata)
				if *removeThresh > 0 && len(PulledNew) > *removeThresh {
					RemoveImages(PulledNew[0:*removeThresh], imageToMDMap)
					PulledNew = PulledNew[*removeThresh:]
				}
				pulledImages[ImageIDType(metadata.Image)] = true
				if len(pulledImages) == IMAGEBATCH {
					break
				}
			}
			if len(pulledImages) > 0 {
				// get and save image data for all the images in pulledimages
				// TODO: parse if other outputs are obtained from scripts
				outMapMap := GetImageAllData(pulledImages)
				SaveImageAllData(outMapMap)
				for imageID := range pulledImages {
					processedImages[imageID] = true
				}
				if e := persistImageList(pulledImages); e != nil {
					blog.Error(e, "Failed to persist list of collected images")
				}
			} else {
				break
			}
		}
	} else {
		blog.Info("Nothing new in this iteration")
	}
	return
}

// getImageList reads the list of previously processed images from the imageList file.
func getImageList(processedImages ImageSet) (e error) {
	f, e := os.Open(*imageList)
	if e != nil {
		blog.Warn(e, ": Error in opening", *imageList, ": perhaps a fresh start?")
		return
	}
	defer f.Close()
	r := bufio.NewReader(f)
	data, e := ioutil.ReadAll(r)
	if e != nil {
		blog.Error(e, ": Error in reading file ", *imageList)
		return
	}
	for _, str := range strings.Split(string(data), "\n") {
		if len(str) != 0 {
			blog.Debug("Previous image: %s", str)
			processedImages[ImageIDType(str)] = true
		}
	}
	return
}

// persistImageList saves the set of processed images to the imageList file.
func persistImageList(collectedImages ImageSet) (e error) {
	var f *os.File
	f, e = os.OpenFile(*imageList, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if e != nil {
		return
	}
	defer f.Close()
	for image := range collectedImages {
		_, e = f.WriteString(string(image) + "\n")
		if e != nil {
			return
		}
	}
	return
}

func printExampleUsage() {
	fmt.Fprintf(os.Stderr, "\n  Examples:\n")
	fmt.Fprintf(os.Stderr, "  (a) Running when compiled from source (standalone mode):\n")
	fmt.Fprintf(os.Stderr, "  \tcd <COLLECTOR_SOURCE_DIR>\n")
	fmt.Fprintf(os.Stderr, "  \tsudo COLLECTOR_DIR=$PWD $GOPATH/bin/collector index.docker.io banyanops/nginx\n\n")
	fmt.Fprintf(os.Stderr, "  (b) Running inside a Docker container: \n")
	fmt.Fprintf(os.Stderr, "  \tsudo docker run --rm \\ \n")
	fmt.Fprintf(os.Stderr, "  \t\t-v ~/.dockercfg:/root/.dockercfg \\ \n")
	fmt.Fprintf(os.Stderr, "  \t\t-v /var/run/docker.sock:/var/run/docker.sock \\ \n")
	fmt.Fprintf(os.Stderr, "  \t\t-v $HOME/.banyan:/banyandir \\ \n")
	fmt.Fprintf(os.Stderr, "  \t\t-v <USER_SCRIPTS_DIR>:/banyancollector/data/userscripts \\ \n")
	fmt.Fprintf(os.Stderr, "  \t\t-e BANYAN_HOST_DIR=$HOME/.banyan \\ \n")
	fmt.Fprintf(os.Stderr, "  \t\tbanyanops/collector index.docker.io banyanops/nginx\n\n")
}

// doFlags defines the cmdline Usage string and parses flag options.
func doFlags() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "  Usage: %s [OPTIONS] REGISTRY REPO [REPO...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n  REGISTRY:\n")
		fmt.Fprintf(os.Stderr, "\tURL of your Docker registry; use index.docker.io for Docker Hub \n")
		fmt.Fprintf(os.Stderr, "\n  REPO:\n")
		fmt.Fprintf(os.Stderr, "\tOne or more repos to gather info about; if no repo is specified Collector will gather info on *all* repos in the Registry\n")
		fmt.Fprintf(os.Stderr, "\n  Environment variables:\n")
		fmt.Fprintf(os.Stderr, "\tCOLLECTOR_DIR:   (Required) Directory that contains the \"data\" folder with Collector default scripts, e.g., $GOPATH/src/github.com/banyanops/collector\n")
		fmt.Fprintf(os.Stderr, "\tCOLLECTOR_ID:    ID provided by Banyan web interface to register Collector with the Banyan service\n")
		fmt.Fprintf(os.Stderr, "\tBANYAN_HOST_DIR: Host directory mounted into Collector/Target containers where results are stored (default: $HOME/.banyan)\n")
		fmt.Fprintf(os.Stderr, "\tBANYAN_DIR:      (Specify only in Dockerfile) Directory in the Collector container where host directory BANYAN_HOST_DIR is mounted\n")
		printExampleUsage()
		fmt.Fprintf(os.Stderr, "  Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if COLLECTORDIR() == "" {
		flag.Usage()
		os.Exit(1)
	}
	if len(flag.Args()) < 1 {
		flag.Usage()
		os.Exit(1)
	}
	if *dockerProto != "unix" && *dockerProto != "tcp" {
		flag.Usage()
		os.Exit(1)
	}
	requiredDirs := []string{BANYANDIR(), filepath.Dir(*imageList), filepath.Dir(*repoList), *banyanOutDir, defaultScriptsDir, userScriptsDir, binDir}
	for _, dir := range requiredDirs {
		blog.Debug("Creating directory: " + dir)
		err := createDirIfNotExist(dir)
		if err != nil {
			blog.Exit(err, ": Error in creating a required directory: ", dir)
		}
	}
	registryspec = flag.Arg(0)
}

// checkRepoList gets the list of repositories to process from the command line
// and from the repoList file.
func checkRepoList() {
	reposToProcess = make(map[RepoType]bool)
	// check repositories specified on the command line
	if len(flag.Args()) > 1 {
		for _, repo := range flag.Args()[1:] {
			reposToProcess[RepoType(repo)] = true
		}
	}
	// check repositories specified in the repoList file. Ignore file read errors.
	data, err := ioutil.ReadFile(*repoList)
	if err != nil {
		blog.Info("Repolist: " + *repoList + " not specified")
		return
	}

	arr := strings.Split(string(data), "\n")
	for _, line := range arr {
		// skip over comments and whitespace
		arr := strings.Split(line, "#")
		repo := arr[0]
		repotrim := strings.TrimSpace(repo)
		if repotrim != "" {
			reposToProcess[RepoType(repotrim)] = true
		}
	}

	if len(reposToProcess) > 0 {
		filterRepos = true
		blog.Info("Limiting collection to the following repos:")
		for repo := range reposToProcess {
			blog.Info(repo)
		}
	}
}

func setOutputWriters(authToken string) {
	dests := strings.Split(*dests, ",")
	for _, dest := range dests {
		var writer Writer
		switch dest {
		case "file":
			writer = newFileWriter("json", *banyanOutDir)
		case "banyan":
			writer = newBanyanWriter(authToken)
		default:
			blog.Error("No such output writer!")
			//ignore the rest and keep going
			continue
		}
		writerList = append(writerList, writer)
	}
}

func setupLogging() {
	blog.AddFilter("stdout", CONSOLELOGLEVEL, blog.NewConsoleLogWriter())
	f, e := os.OpenFile(LOGFILENAME, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if e != nil {
		blog.Exit(e, ": Error in opening log file: ", LOGFILENAME)
	}
	f.Close()
	flw := blog.NewFileLogWriter(LOGFILENAME, false)
	blog.AddFilter("file", FILELOGLEVEL, flw)
}

// copyBanyanData copies all the default scripts and binaries (e.g., bash-static, python-static, etc.)
// to BANYANDIR (so that it can be mounted into collector/target containers)
func copyBanyanData() {
	copyDir(COLLECTORDIR()+"/data/defaultscripts", defaultScriptsDir)
	//copy scripts from user specified/default directory to userScriptsDir for mounting
	copyDir(*userScriptStore, userScriptsDir)
	// * needed to copy into binDir (rather than a subdir called bin)
	copyDirTree(COLLECTORDIR()+"/data/bin/*", binDir)
}

func main() {
	doFlags()

	setupLogging()

	//verifyVolumes()

	copyBanyanData()

	// setup connection to docker unix socket
	var e error
	DockerTransport, e = NewDockerTransport(*dockerProto, *dockerAddr)
	if e != nil {
		blog.Exit(e, ": Error in connecting to docker remote API socket")
	}

	registryAPIURL, hubAPI, XRegistryAuth = getRegistryURL()
	blog.Info("registry API URL: %s", registryAPIURL)
	authToken := registerCollector()

	// Set output writers
	setOutputWriters(authToken)

	// Images we have processed already
	processedImages := NewImageSet()
	e = getImageList(processedImages)
	if e != nil {
		blog.Info("Fresh start: No previously collected images were found in %s", *imageList)
	}
	blog.Debug(processedImages)

	// Image Metadata we have already seen
	ImiSet := NewImiSet()
	PulledList := []ImageMetadataInfo{}

	duration := time.Duration(*poll) * time.Second

	// Main infinite loop.
	for {
		checkRepoList()
		ImiSet, PulledList = DoIteration(authToken, processedImages, ImiSet, PulledList)

		blog.Info("Looping in %d seconds", *poll)
		time.Sleep(duration)
	}
}
