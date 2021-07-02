package main

import (
	"bytes"
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	//"context"
	"encoding/json"
	"flag"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"log"
	"net/http"
	"text/template"

	//"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	//v1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/client-go/util/homedir"
	"time"

	"github.com/kelseyhightower/envconfig"
	//"log"
	"path/filepath"
	//"github.com/kelseyhightower/envconfig"

	//"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/rest"
)

// TODO
// x check if a pod actually belongs to a deployment, before trying to patch it, then check if that deployment exists
// x add a teams alert message, make it interactive ??
// x change delay to 1 minute
// x check the label, if IGNOREME=true  exists, then do so
// add chart for rbac, serviceaccount etc
// add gauges for Datadog to scrape,  this'll show the number of restarts in a time frame and allow pagerduty alerts
// check deploy # and that all are failing.  maybe just one node is having issuest
// Change the "seen" mechanism.   log the restart count on first time of running,  and the timestamp.(if > 0)
// each pass checks the restart count and if its "start_count" + cfg.RestartCount within a timeframe(int64) then take action

//setup global vars here
type Config struct {
	WebhookURL    string     `default:"unknown"`
	RestartCount  int32
	rescan        int
	TimeIgnore    int
}

type failedPodTemplateData struct {
	Comment       string
	ClusterName   string
	Errmsg        string
	DeployName    string
	PodName       string
	PodNamespace  string
	Restarts      int32
}

type failingDeployment struct {
	podName       string
	restarts      int32
}

type failingPod struct {
	DeployName       string
	currRestartCount int32
	initialRestarts  int32
	NodeName         string
	firstSeen        int64
	lastSeen         int64
}

type key struct {
	name string
	ns   string
}

var seen = map[string] int64{}
var failingPods = map[key] failingPod{}
var badDeployments = map[string] failingDeployment{}

var localMode bool
var killMode bool
var cfg Config

var gauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "k8smonitoring",
		Name:      "failingpod",
		Help:      "number of times restarted",
	})

func check(e error) {
	if e != nil {
		panic(e)
	}
}

/*
Function sets up kubernetes clientset for the given cluster either using a local kubeconfig file
or by getting access to the cluster itself
This relies on -localMode flag
*/
func getClientSet() *kubernetes.Clientset {

	var config *rest.Config
	var err error

	if localMode {
		fmt.Print("Using clients KubeConfig(local mode)")
		var kubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		fmt.Print("..........based on cluster config")
		// relies on injection of /var/run/secrets/kubernetes.io/serviceaccount
		config, err = rest.InClusterConfig()
	}
	fmt.Println()
	check(err)

	// create the clientset , in future use the incluster config...no certs required
	clientset, err := kubernetes.NewForConfig(config)
	check(err)

	return clientset

}

/*
Function allows us to use a json template file and throw data at it
*/
func parseJsonTemplate(a string,b failedPodTemplateData) string  {
	t, err := template.ParseFiles(a)
	if err != nil {
		panic(err)
	}
	buf := &bytes.Buffer{}
	err = t.Execute(buf, b)
	if err != nil {
		panic(err)
	}
	s := buf.String()
	return s
}

/*
Function to send a Teams notification to a specific Webhook (Team/Channel)
*/
func sendTeamsNotification(webhookUrl string, payload string) error {
	resp, err := http.Post(webhookUrl, "application/json", bytes.NewBuffer([]byte(payload)))
	if err != nil {
		log.Fatal("Error sending Teams notification",err)
	}
	defer resp.Body.Close()
	return nil
}

/*
Function to Scale a Kubernetes Deployment down to zero replicas, leaving the config intact.
It also labels the deployment with another patch, to make identification easier
*/
func takeDeploymentsToZero(cs *kubernetes.Clientset, podName string, deployName string, podNamespace string, restarts int32) {
	// we identify the deployment name by the label ...   app=xx  , check for a better way to show the parent
	type patchStringValue struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}

	type patchUInt32Value struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value uint32 `json:"value"`
	}
	payload := []patchUInt32Value{{
		Op:    "replace",
		Path:  "/spec/replicas",
		Value: 0,
	}}
	labelpayload := []patchStringValue{{
		Op:    "replace",
		Path:  "/metadata/labels/ClusterScaledToZero",
		Value: "true",
	}}
	payloadBytes, _ := json.Marshal(payload)
	labelpayloadBytes, _ := json.Marshal(labelpayload)
	fmt.Printf("Target acquired ... POD(%s) DEPLOY(%s) NAMESPACE(%s) Restarts(%d)  <--- Scaling it down to 0\n",podName,deployName,podNamespace,restarts)
	if ! killMode {
		fmt.Println("Taking no further action, killmode not engaged")
		return
	}

	// Example left here for posterity
	//ctx, cancel := context.WithCancel(context.Background())
	//defer cancel()
	if deployName != "" {
		// First lets scale it to zero
		_, err := cs.AppsV1().Deployments(podNamespace).Patch(context.Background(), deployName, types.JSONPatchType, payloadBytes,metav1.PatchOptions{})
		if err != nil {
			panic(err.Error())
		}

		// Now lets label it as being scaled down
		_, err2 := cs.AppsV1().Deployments(podNamespace).Patch(context.Background(), deployName, types.JSONPatchType, labelpayloadBytes,metav1.PatchOptions{})
		if err2 != nil {
			panic(err2.Error())
		}
		// set the seen variable = true
		now := time.Now()
		//fmt.Println("Found ", now)
		seen[deployName] = now.Unix()
		td := failedPodTemplateData{Comment: "Failed Deployment", ClusterName: "Clusteryyy", Errmsg: "This Deployment has been scaled to zero after several failures", DeployName: deployName, PodName: podName, PodNamespace: podNamespace, Restarts: restarts}
		payload := parseJsonTemplate("./templates/teams-alert-failed-deployments.json", td)

		err3 := sendTeamsNotification(cfg.WebhookURL, payload)
		if err != nil {
			log.Fatal("Couldn't send the Teams message: ", err3)
		}
	} else {
		// XXXX   call  unidentified_pod()
		//fmt.Printf("There is a failing pod (%s) restart(%d) which has no label app=xx\n",podName,restarts)
		//td := failedPodTemplateData{Comment: "Failed Pod", ClusterName: "Clusterxxx", Errmsg: "This pod is repeatedly failing, but can't be scaled to zero as its not part of a deployment", PodName: podName, PodNamespace: podNamespace, Restarts: restarts}
		//payload := parseJsonTemplate("./templates/teams-alert-failed-pods.json", td)
		//
		//err := sendTeamsNotification(cfg.WebhookURL, payload)
		//if err != nil {
		//	log.Fatal("Couldn't send the Teams message: ", err)
		//}
		// handle this with an alert and a metric which will highlight just failing pods which aren't part of a deployment

		//options := metav1.ListOptions{
		//	LabelSelector: "app=<APPNAME>",
		//}
	}
}

/*
Display startup splashscreen with handy information on how to interact with the controller
 */
func printRobot() {
	fmt.Println("┌-------------------------------------------------------------------------------------------------------------------------------------------------┐")
	fmt.Println("| Starting - Kube restart controller, I look for constantly restarting pods and scale them to zero. Anything over 3 restarts is my primary target |")
	fmt.Println("| I will scale the deployment to zero, and also add a label, \"ClusterScaledToZero=true\" , this can be found by running: -                         |")
	fmt.Println("|                                                                                                                                                 |")
	fmt.Println("| kubectl get deployments --all-namespaces --show-labels -l=ClusterScaledToZero=true                                                              |")
	fmt.Println("|                                                                                                                                                 |")
	fmt.Println("| You can scale the deployment back up again by running                                                                                           |")
	fmt.Println("| kubectl scale deployment xx --replicas=2                                                                                                        |")
	fmt.Println("| kubectl label deployment xx ClusterScaledToZero-                                                                                                |")
	fmt.Println("|                                                                                                                                                 |")
	fmt.Println("| Deployments can be ignored by labelling them                                                                                                    |")
	fmt.Println("| kubectl label pod xxx ignoreme=true                                                                                                             |")
	fmt.Println("└-------------------------------------------------------------------------------------------------------------------------------------------------┘")

	fmt.Printf("       _______\n     _/       \\_\n    / |       | \\\n   /  |__   __|  \\\n  |__/((o| |o))\\__|\n  |      | |      |\n  |\\     |_|     /|\n  | \\           / |\n   \\| /  ___  \\ |/\n    \\ | / _ \\ | /\n     \\_________/\n      _|_____|_\n ____|_________|____\n/                   \\  -- Kill mode %t ...\n",killMode)

}

/*
Return a list of all pods in the cluster
 */
func listAllPods(cs *kubernetes.Clientset) *v1.PodList{
	pods, err := cs.CoreV1().Pods("reference-data").List( context.Background(),metav1.ListOptions{} )
	if err != nil {
		panic(err.Error())
	}
	return pods
}

/*
return the owner.Name of the Controlling owner, there can be multiple owners, but only 1 is the controller
 */
func getOwnerRefs(ref []metav1.OwnerReference, refType string) string{
	for _, owner := range ref {
		// its possible to have multiple owners of an object, we only want the controller
		if *owner.Controller {
			// we only want to deal with ReplicaSets or Deployments, these are passed in a refType
			if owner.Kind == refType {
				//fmt.Printf("GGG %s\n", owner.Name)
				return owner.Name
			}
		}
	}
	return ""
}

/*
return the owner of the RecordSet
 */
func getRecordsetOwner(cs *kubernetes.Clientset,ns string, rsName string) string{
	// Get the owner for a specific recordset in a namespace, i.e a deployment name
	rs, err := cs.AppsV1().ReplicaSets(ns).Get(context.Background(), rsName, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}

	ownerName := getOwnerRefs(rs.GetObjectMeta().GetOwnerReferences(),"Deployment")
	return ownerName
}

/*
return the last failure time for a pod
 */
func getLastFailTime(status v1.ContainerStatus) metav1.Time{
	x := metav1.NewTime(time.Now())
	t := status.LastTerminationState.Terminated
	if t != nil {
		return t.FinishedAt
	}
	return x
}

/*
Function is polled every cfg.rescan seconds, and checks which pods are restarting.

On the initial run, it stores the current restart counters, to take account of historic restarts.

If a pod restarts more than cfg.RestartCount times in cfg.TimeIgnore seconds, then it is targeted
and scaled down to zero, and a pod label attached to denote the action.
 */
func checkPodStatus(cs *kubernetes.Clientset, firstPass bool) {
	if firstPass {
		fmt.Println("Loading Initial pod restart count - first run")
	} else {
		fmt.Println("Looping over pod restart counts")
	}

// test cases
//  1. pod at startup has some restarts already
//  XX 2. pod is deleted and it disappears.    if entries in "failingPods" not in "pods" , delete them
//  3. currRestarts - initialrestarts > cfg.RestartCount then check further
//       a. are all pods in this deployment failing, or just 1 or 2 ?
//       b. if all,  then scale the deployment down to zero


	pods:= listAllPods(cs)
	if ! firstPass {
		// loop through pods,  and remove any "failingPods" that dont exist any more, first lets create a lookup of current pods
		var allPods = map[key] int32{}
		for _, pod := range pods.Items {
			podKey := key{name: pod.Name, ns: pod.Namespace}
			allPods[podKey] = 1
		}

		//now lets loop through failingPods and remove any that have been deleted
		for k,_ := range failingPods {
			if val, ok := allPods[k]; ! ok {
				fmt.Println("Found old key and deleting it ",k,val)
				delete(failingPods,k)
			}
		}
	}
	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.RestartCount > cfg.RestartCount {
				now := time.Now()

				// get time of last failure
				//lastFail := getLastFailTime(status)
				fmt.Printf("kubectl -n %s get pod %s -ojson\n",pod.Namespace,pod.Name)
				lastFail := getLastFailTime(status)

				fmt.Printf("Last failure time Pod(%s) NS(%s) Time %s\n",pod.Name, pod.Namespace, lastFail)
				rsOwnerName := getOwnerRefs(pod.GetObjectMeta().GetOwnerReferences(),"ReplicaSet")

				failedPodKey := key{name: pod.Name, ns: pod.Namespace}
				// here we'll create a new entry,  but copy over the original "firstseen" if it exists
				failedPodVal := failingPod{DeployName: "", currRestartCount: status.RestartCount, NodeName: pod.Status.HostIP}
				if firstPass {
					failedPodVal.firstSeen = now.Unix()
					failedPodVal.initialRestarts = status.RestartCount
				} else {
					failedPodVal.lastSeen = now.Unix()
					failedPodVal.firstSeen = failingPods[failedPodKey].firstSeen
					failedPodVal.initialRestarts = failingPods[failedPodKey].initialRestarts
				}

				if rsOwnerName != "" {
					dpOwnerName := getRecordsetOwner(cs, pod.Namespace, rsOwnerName)
					if dpOwnerName != "" {
						// XX remove me
						//if dpOwnerName != "badpod" {
						//	continue
						//}
						//fmt.Printf("POD owned by an RS named (%s) - RS owned by a DEPLOY named (%s)\n", rsOwnerName, dpOwnerName)
						fmt.Printf("Pod Failure in Deployment - Name(%s) Namespace(%s) Restart(%d) Deployment(%s) RS(%s) NodeIP(%s) Time(%d)\n",
							pod.Name, pod.Namespace, status.RestartCount, dpOwnerName, rsOwnerName, pod.Status.HostIP, now.Unix())
						failedPodVal.DeployName = dpOwnerName
						failingPods[failedPodKey] = failedPodVal
					} else {
						// failing pod that's in an RS that's not part of a Deploy, include it with no deployment name
						fmt.Printf("Pod Failure - Name(%s) Namespace(%s) Restart(%d) NodeIP(%s) Time(%d)\n",pod.Name,pod.Namespace,status.RestartCount, pod.Status.HostIP, now.Unix())
						failingPods[failedPodKey] = failedPodVal
					}
				} else {
					// just a pods failing that's not part of an RS, include it with no deployment name
					fmt.Printf("Pod Failure - Name(%s) Namespace(%s) Restart(%d) NodeIP(%s) Time(%d)\n",pod.Name,pod.Namespace,status.RestartCount, pod.Status.HostIP, now.Unix())
					failingPods[failedPodKey] = failedPodVal
				}
			}
		}
	}

	//for _, pod := range pods.Items {
	//	//fmt.Printf("Name #%d %s - %s\n", i, pod.Name, pod.Namespace)
	//	for _, status := range pod.Status.ContainerStatuses {
	//		//fmt.Printf("\tXXXXXXXXX Pod %d %s - Namespace %s\nOWNER %s\n\n",x,pod.Name,pod.Namespace,pod.Labels["app"])
	//		// has this deployment been SEEN yet ? and has it been 60 seconds or longer since we tried to patch it ?
	//		// should we ignore this pod, is it labelled to be ignored ?
	//		if status.RestartCount > cfg.RestartCount {
	//			if pod.Labels["ignoreme"] != "true" {
	//				if val, ok := seen[pod.Labels["app"]]; ok {
	//					now := time.Now()
	//					secs := now.Unix()
	//					if (secs-val > 60) {
	//						takeDeploymentsToZero(cs, pod.Name, pod.Labels["app"], pod.Namespace, status.RestartCount)
	//					}
	//				} else {
	//					fmt.Printf("NOT SEEN SO FAR %s %s\n",pod.Name,pod.Namespace)
	//					takeDeploymentsToZero(cs, pod.Name, pod.Labels["app"], pod.Namespace, status.RestartCount)
	//				}
	//			} else {
	//				fmt.Printf("Ignoring the deployment %s\n", pod.Labels["app"])
	//			}
	//		}
	//	}
	//}
	//// lets loop through SEEN, and delete anything older than 60 seconds, then it can be re-attempted if needed
	//for k, v := range seen {
	//	fmt.Printf("\tDeployments in process of being scaled down %s -> %d\n", k, v)
	//	now := time.Now()
	//	secs := now.Unix()
	//	if (secs - v > 30 ) {
	//		delete(seen,k)
	//	}
	//}
	fmt.Println()
	printFailingPods()
}

func printFailingPods() {
	for k,v := range failingPods {
		fmt.Printf("Seen Pod(%s) NS(%s) InitialRestarts(%d) CurrRestarts(%d) Deploy(%s) Node(%s) FirstSeen(%d) LastSeen(%d)\n",k.name,k.ns,v.initialRestarts,v.currRestartCount,v.DeployName,v.NodeName,v.firstSeen,v.lastSeen)
	}
}

func pollFailingPods() {
	// Connect to the k8s API, check CLI flags,  and setup timed loop to check or failing pods
	var cs *kubernetes.Clientset

	cfg.WebhookURL = "https://maersk.webhook.office.com/webhookb2/cd1feb44-7662-46d8-a725-0c50128a47ca@05d75c05-fa1a-42e7-9cf1-eb416c396f2d/IncomingWebhook/008a64cc4c584dc3bdc07ecd39daacd4/7f186295-a378-4f9b-ad2c-40417479f303"
	cfg.RestartCount = 1
	cfg.rescan = 10
	cfg.TimeIgnore = 100

	flag.BoolVar(&localMode, "l", false, "Turn on local running mode")
	flag.BoolVar(&killMode, "k", false, "engage kill mode")
	flag.Parse()

	cs = getClientSet()
	printRobot()
	// At first pass, get existing restart counts so we don't kill anything that has a # > 0
	checkPodStatus(cs,true)
	time.Sleep(time.Duration(cfg.rescan) * time.Second)

	// main timed loop to repeatedly scan for failing pods
	for {
		go checkPodStatus(cs,false)
		time.Sleep(time.Duration(cfg.rescan) * time.Second)
	}
}

func twoSum(nums []int, target int) []int {
	for i := 0; i < len(nums); i++ {
		fmt.Printf("Checking I %d val(%d)\n",i,nums[i])
		for x := i+1; x < len(nums); x++ {
			fmt.Printf("\tChecking X%d %d\n", x, nums[x])
			if nums[i] + nums[x] == target {
				fmt.Println("XXXX")
				return []int {i,x}
			}
		}
	}

	return []int {}
}


func main() {
	// Setup the Environment variables
	err := envconfig.Process("", &cfg)
	if err != nil {
		log.Fatal(err.Error())
	}

	fmt.Printf("Starting with a Rescan time (%d) \n", cfg.rescan)
	prometheus.MustRegister(gauge)
	go pollFailingPods()  //check the endpoints every cfg.timeout seconds

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":80", nil)
}