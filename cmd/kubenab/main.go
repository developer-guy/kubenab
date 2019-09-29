package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"k8s.io/api/admission/v1beta1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpPingRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_ping_requests_total",
		Help: "Count of all HTTP requests on '/ping'",
	}, []string{"code", "method"})

	httpRequestDuration = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name:        "http_request_duration_milliseconds",
		Help:        "The HTTP request Duration in Milliseconds",
		// Latency Distribution
		Objectives:  map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	}, []string{"api_method"})
)

var (
	tlsCertFile string
	tlsKeyFile  string
)

var (
	dockerRegistryUrl     = os.Getenv("DOCKER_REGISTRY_URL")
	registrySecretName    = os.Getenv("REGISTRY_SECRET_NAME")
	whitelistRegistries   = os.Getenv("WHITELIST_REGISTRIES")
	whitelistNamespaces   = os.Getenv("WHITELIST_NAMESPACES")
	whitelistedNamespaces = strings.Split(whitelistNamespaces, ",")
	whitelistedRegistries = strings.Split(whitelistRegistries, ",")
)

type patch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func main() {
	// check if all required Flags are set and in a correct Format
	checkArguments()

	flag.StringVar(&tlsCertFile, "tls-cert", "/etc/admission-controller/tls/tls.crt", "TLS certificate file.")
	flag.StringVar(&tlsCertFile, "tls-cert", "/etc/admission-controller/tls/tls.crt", "TLS certificate file.")
	flag.StringVar(&tlsKeyFile, "tls-key", "/etc/admission-controller/tls/tls.key", "TLS key file.")
	flag.Parse()

	promRegistry := prometheus.NewRegistry()
	promRegistry.MustRegister(httpPingRequestsTotal)
	promRegistry.MustRegister(httpRequestDuration)
	http.HandleFunc("/mutate", mutateAdmissionReviewHandler)
	http.HandleFunc("/validate", validateAdmissionReviewHandler)
	s := http.Server{
		Addr: ":443",
		TLSConfig: &tls.Config{
			ClientAuth: tls.NoClientCert,
		},
	}
	log.Fatal(s.ListenAndServeTLS(tlsCertFile, tlsKeyFile))
}

func mutateAdmissionReviewHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	//set header
	w.Header().Set("Content-Type", "application/json")

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Println(string(data))

	ar := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(data, &ar); err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	namespace := ar.Request.Namespace
	log.Printf("AdmissionReview Namespace is: %s", namespace)

	admissionResponse := v1beta1.AdmissionResponse{Allowed: false}
	patches := []patch{}
	if !contains(whitelistedNamespaces, namespace) {
		pod := v1.Pod{}
		if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Handle Containers
		for _, container := range pod.Spec.Containers {
			createPatch := handleContainer(&container, dockerRegistryUrl)
			if createPatch {
				patches = append(patches, patch{
					Op:    "add",
					Path:  "/spec/containers",
					Value: []v1.Container{container},
				})
			}
		}

		// Handle init containers
		for _, container := range pod.Spec.InitContainers {
			createPatch := handleContainer(&container, dockerRegistryUrl)
			if createPatch {
				patches = append(patches, patch{
					Op:    "add",
					Path:  "/spec/initContainers",
					Value: []v1.Container{container},
				})
			}
		}
	} else {
		log.Printf("Namespace is %s Whitelisted", namespace)
	}

	admissionResponse.Allowed = true
	if len(patches) > 0 {

		// Add image pull secret patche
		patches = append(patches, patch{
			Op:   "add",
			Path: "/spec/imagePullSecrets",
			Value: []v1.LocalObjectReference{
				v1.LocalObjectReference{
					Name: registrySecretName,
				},
			},
		})

		patchContent, err := json.Marshal(patches)
		if err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		admissionResponse.Patch = patchContent
		pt := v1beta1.PatchTypeJSONPatch
		admissionResponse.PatchType = &pt
	}

	ar = v1beta1.AdmissionReview{
		Response: &admissionResponse,
	}

	data, err = json.Marshal(ar)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func handleContainer(container *v1.Container, dockerRegistryUrl string) bool {
	log.Println("Container Image is", container.Image)

	if !containsRegisty(whitelistedRegistries, container.Image) {
		message := fmt.Sprintf("Image is not being pulled from Private Registry: %s", container.Image)
		log.Printf(message)

		imageParts := strings.Split(container.Image, "/")
		newImage := ""
		if len(imageParts) < 3 {
			newImage = dockerRegistryUrl + "/" + container.Image
		} else {
			imageParts[0] = dockerRegistryUrl
			newImage = strings.Join(imageParts, "/")
		}
		log.Printf("Changing image registry to: %s", newImage)

		container.Image = newImage
		return true
	} else {
		log.Printf("Image is being pulled from Private Registry: %s", container.Image)
	}
	return false
}

func validateAdmissionReviewHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	//set header
	w.Header().Set("Content-Type", "application/json")

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Println(string(data))

	ar := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(data, &ar); err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	namespace := ar.Request.Namespace
	log.Printf("AdmissionReview Namespace is: %s", namespace)

	admissionResponse := v1beta1.AdmissionResponse{Allowed: false}
	if !contains(whitelistedNamespaces, namespace) {
		pod := v1.Pod{}
		if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Handle containers
		for _, container := range pod.Spec.Containers {
			log.Println("Container Image is", container.Image)

			if !containsRegisty(whitelistedRegistries, container.Image) {
				message := fmt.Sprintf("Image is not being pulled from Private Registry: %s", container.Image)
				log.Printf(message)
				admissionResponse.Result = getInvalidContainerResponse(message)
				goto done
			} else {
				log.Printf("Image is being pulled from Private Registry: %s", container.Image)
				admissionResponse.Allowed = true
			}
		}

		// Handle init containers
		for _, container := range pod.Spec.InitContainers {
			log.Println("Init Container Image is", container.Image)

			if !containsRegisty(whitelistedRegistries, container.Image) {
				message := fmt.Sprintf("Image is not being pulled from Private Registry: %s", container.Image)
				log.Printf(message)
				admissionResponse.Result = getInvalidContainerResponse(message)
				goto done
			} else {
				log.Printf("Image is being pulled from Private Registry: %s", container.Image)
				admissionResponse.Allowed = true
			}
		}
	} else {
		log.Printf("Namespace is %s Whitelisted", namespace)
		admissionResponse.Allowed = true
	}

done:
	ar = v1beta1.AdmissionReview{
		Response: &admissionResponse,
	}

	data, err = json.Marshal(ar)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func getInvalidContainerResponse(message string) *metav1.Status {
	return &metav1.Status{
		Reason: metav1.StatusReasonInvalid,
		Details: &metav1.StatusDetails{
			Causes: []metav1.StatusCause{
				{Message: message},
			},
		},
	}
}

// if current namespace is part of whitelisted namespaces
func contains(arr []string, str string) bool {
	for _, a := range arr {
		if a == str || strings.Contains(a, str) {
			return true
		}
	}
	return false
}

// if current registry is part of whitelisted registries
func containsRegisty(arr []string, str string) bool {
	for _, a := range arr {
		if a == str || strings.Contains(str, a) {
			return true
		}
	}
	return false
}

// ping responds to the request with a plain-text "Ok" message.
func healthCheck(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	fmt.Fprintf(w, "Ok")
}
