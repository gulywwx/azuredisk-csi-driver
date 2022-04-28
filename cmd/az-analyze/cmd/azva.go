/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1beta1 "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1beta1"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"
	consts "sigs.k8s.io/azuredisk-csi-driver/pkg/azureconstants"
)

// azvaCmd represents the azva command
var azvaCmd = &cobra.Command{
	Use:   "azva",
	Short: "Azure Volume Attachment",
	Long:  `Azure Volume Attachment is a Kubernetes Custom Resource.`,
	Run: func(cmd *cobra.Command, args []string) {
		pod, _ := cmd.Flags().GetString("pod")
		node, _ := cmd.Flags().GetString("node")
		zone, _ := cmd.Flags().GetString("zone")
		namespace, _ := cmd.Flags().GetString("namespace")

		numFlag := cmd.Flags().NFlag()
		if hasNamespace := namespace != ""; hasNamespace {
			numFlag--
		}

		// access to config and Clientsets
		config := getConfig()
		clientsetK8s := getKubernetesClientset(config)
		clientsetAzDisk := getAzDiskClientset(config)

		var result []AzvaResource

		if numFlag > 1 {
			fmt.Printf("only one of the flags is allowed.\n" + "Run 'az-analyze --help' for usage.\n")
		} else {
			if numFlag == 0 {
				// if no flag value is provided , list all of the pods/nodes/zone information
				resultAll := GetAllAzVolumeAttachements(clientsetK8s, clientsetAzDisk, namespace)
				if len(resultAll) != 0 {
					displayAzvaAll(resultAll)
				} else {
					fmt.Println("No azVolumeAttachment was found")
				}
			} else if pod != "" {
				result = GetAzVolumeAttachementsByPod(clientsetK8s, clientsetAzDisk, pod, namespace)
				if len(result) != 0 {
					displayAzva(result, "POD")
				} else {
					fmt.Println("No azVolumeAttachment was found")
				}
			} else if node != "" {
				result = GetAzVolumeAttachementsByNode(clientsetAzDisk, node)
				if len(result) != 0 {
					displayAzva(result, "NODE")
				} else {
					fmt.Println("No azVolumeAttachment was found")
				}
			} else if zone != "" {
				result = GetAzVolumeAttachementsByZone(clientsetK8s, clientsetAzDisk, zone)
				if len(result) != 0 {
					displayAzva(result, "ZONE")
				} else {
					fmt.Println("No azVolumeAttachment was found")
				}
			}
		}
	},
}

func init() {
	getCmd.AddCommand(azvaCmd)
	azvaCmd.PersistentFlags().StringP("pod", "p", "", "insert-pod-name (only one of the flags is allowed).")
	azvaCmd.PersistentFlags().StringP("node", "d", "", "insert-node-name (only one of the flags is allowed).")
	azvaCmd.PersistentFlags().StringP("zone", "z", "", "insert-zone-name (only one of the flags is allowed).")
	azvaCmd.PersistentFlags().StringP("namespace", "n", "", "insert-namespace (optional).")
}

type AzvaResource struct {
	ResourceType string
	Namespace    string
	Name         string
	Age          time.Duration
	RequestRole  v1beta1.Role
	Role         v1beta1.Role
	State        v1beta1.AzVolumeAttachmentAttachmentState
}

type AzvaResourceAll struct {
	PodName     string
	NodeName    string
	ZoneName    string
	Namespace   string
	Name        string
	Age         time.Duration
	RequestRole v1beta1.Role
	Role        v1beta1.Role
	State       v1beta1.AzVolumeAttachmentAttachmentState
}

// return azVolumeAttachements with all Pods/Nodes/Zones when no flags is provided
func GetAllAzVolumeAttachements(clientsetK8s kubernetes.Interface, clientsetAzDisk versioned.Interface, namespace string) []AzvaResourceAll {
	result := make([]AzvaResourceAll, 0)

	if namespace == "" {
		namespace = "default"
	}

	// get pvc claim names of pod(s)
	pvcClaimNameSet := make(map[string][]string)

	pods, err := clientsetK8s.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, pod := range pods.Items {
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				pvcClaimNameSet[v.PersistentVolumeClaim.ClaimName] = append(pvcClaimNameSet[v.PersistentVolumeClaim.ClaimName], pod.Name)
			}
		}
	}

	// get azVolumeAttachments with the same claim name in pvcClaimNameSet
	azVolumeAttachments, err := clientsetAzDisk.DiskV1beta1().AzVolumeAttachments(getDriverNamesapce()).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, azVolumeAttachment := range azVolumeAttachments.Items {
		pvcClaimName := azVolumeAttachment.Spec.VolumeContext[consts.PvcNameKey]
		nodeName := azVolumeAttachment.Spec.NodeName
		node, err := clientsetK8s.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		if err != nil {
			panic(err.Error())
		}

		zoneName := node.Labels[consts.WellKnownTopologyKey]

		// if pvcClaimName is contained in pvcClaimNameSet, add the azVolumeattachment to result
		if pNames, ok := pvcClaimNameSet[pvcClaimName]; ok {
			for _, pName := range pNames {
				result = append(result, AzvaResourceAll{
					PodName:     pName,
					NodeName:    nodeName,
					ZoneName:    zoneName,
					Namespace:   azVolumeAttachment.Namespace,
					Name:        azVolumeAttachment.Name,
					Age:         metav1.Now().Sub(azVolumeAttachment.CreationTimestamp.Time),
					RequestRole: azVolumeAttachment.Spec.RequestedRole,
					Role:        azVolumeAttachment.Status.Detail.Role,
					State:       azVolumeAttachment.Status.State,
				})
			}
		}
	}
	fmt.Println(result) // debug
	return result
}

// return azVolumeAttachements by pod when pod name is provided
func GetAzVolumeAttachementsByPod(clientsetK8s kubernetes.Interface, clientsetAzDisk versioned.Interface, podName string, namespace string) []AzvaResource {
	result := make([]AzvaResource, 0)

	if namespace == "" {
		namespace = "default"
	}

	// get pvc claim names of pod(s)
	pvcClaimNameSet := make(map[string][]string)

	pod, err := clientsetK8s.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			pvcClaimNameSet[v.PersistentVolumeClaim.ClaimName] = append(pvcClaimNameSet[v.PersistentVolumeClaim.ClaimName], pod.Name)
		}
	}

	// get azVolumeAttachments with the same claim name in pvcClaimNameSet
	azVolumeAttachments, err := clientsetAzDisk.DiskV1beta1().AzVolumeAttachments(getDriverNamesapce()).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, azVolumeAttachment := range azVolumeAttachments.Items {
		pvcClaimName := azVolumeAttachment.Spec.VolumeContext[consts.PvcNameKey]

		// if pvcClaimName is contained in pvcClaimNameSet, add the azVolumeattachment to result
		if pNames, ok := pvcClaimNameSet[pvcClaimName]; ok {
			for _, pName := range pNames {
				result = append(result, AzvaResource{
					ResourceType: pName,
					Namespace:    azVolumeAttachment.Namespace,
					Name:         azVolumeAttachment.Name,
					Age:          metav1.Now().Sub(azVolumeAttachment.CreationTimestamp.Time),
					RequestRole:  azVolumeAttachment.Spec.RequestedRole,
					Role:         azVolumeAttachment.Status.Detail.Role,
					State:        azVolumeAttachment.Status.State,
				})
			}
		}
	}
	fmt.Println(result) // debug
	return result
}

// return azVolumeAttachements by node when node name is provided
func GetAzVolumeAttachementsByNode(clientsetAzDisk versioned.Interface, nodeName string) []AzvaResource {
	result := make([]AzvaResource, 0)

	azVolumeAttachments, err := clientsetAzDisk.DiskV1beta1().AzVolumeAttachments(getDriverNamesapce()).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, azVolumeAttachment := range azVolumeAttachments.Items {
		if azVolumeAttachment.Spec.NodeName == nodeName {
			result = append(result, AzvaResource{
				ResourceType: azVolumeAttachment.Spec.NodeName,
				Namespace:    azVolumeAttachment.Namespace,
				Name:         azVolumeAttachment.Name,
				Age:          metav1.Now().Sub(azVolumeAttachment.CreationTimestamp.Time),
				RequestRole:  azVolumeAttachment.Spec.RequestedRole,
				Role:         azVolumeAttachment.Status.Detail.Role,
				State:        azVolumeAttachment.Status.State,
			})
		}
	}
	fmt.Println(result) // debug
	return result
}

// return azVolumeAttachements by zone when zone name is provided
func GetAzVolumeAttachementsByZone(clientsetK8s kubernetes.Interface, clientsetAzDisk versioned.Interface, zoneName string) []AzvaResource {
	result := make([]AzvaResource, 0)

	// get nodes in the zone
	nodeSet := make(map[string]string)

	nodes, err := clientsetK8s.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, node := range nodes.Items {
		if node.Labels[consts.WellKnownTopologyKey] == zoneName {
			nodeSet[node.Name] = node.Labels[consts.WellKnownTopologyKey]
		}
	}

	// get azVolumeAttachments of the nodes in the zone
	azVolumeAttachments, err := clientsetAzDisk.DiskV1beta1().AzVolumeAttachments(getDriverNamesapce()).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	for _, azVolumeAttachment := range azVolumeAttachments.Items {
		if zName, ok := nodeSet[azVolumeAttachment.Spec.NodeName]; ok {
			result = append(result, AzvaResource{
				ResourceType: zName,
				Namespace:    azVolumeAttachment.Namespace,
				Name:         azVolumeAttachment.Name,
				Age:          metav1.Now().Sub(azVolumeAttachment.CreationTimestamp.Time),
				RequestRole:  azVolumeAttachment.Spec.RequestedRole,
				Role:         azVolumeAttachment.Status.Detail.Role,
				State:        azVolumeAttachment.Status.State,
			})
		}
	}
	fmt.Println(result) // debug
	return result
}

func displayAzva(result []AzvaResource, typeName string) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{strings.ToUpper(typeName) + "NAME", "NAMESPACE", "NAME", "AGE", "REQUESTEDROLE", "ROLE", "STATE"})

	for _, azva := range result {
		table.Append([]string{azva.ResourceType, azva.Namespace, azva.Name, timeFmt(azva.Age), string(azva.RequestRole), string(azva.Role), string(azva.State)})
	}

	table.Render()
}

func displayAzvaAll(result []AzvaResourceAll) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"PODNAME", "NODENAME", "ZONENAME", "NAMESPACE", "NAME", "AGE", "REQUESTEDROLE", "ROLE", "STATE"})

	for _, azva := range result {
		table.Append([]string{azva.PodName, azva.NodeName, azva.ZoneName, azva.Namespace, azva.Name, timeFmt(azva.Age), string(azva.RequestRole), string(azva.Role), string(azva.State)})
	}

	table.Render()
}
