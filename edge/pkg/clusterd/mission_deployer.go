/*
Copyright 2015 The Kubernetes Authors.
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

package clusterd

import (
	"encoding/json"
	"fmt"
	yaml "gopkg.in/yaml.v2"
	"strconv"
	"strings"
	"sync"

	"github.com/kubeedge/beehive/pkg/core/model"
	edgeclustersv1 "github.com/kubeedge/kubeedge/cloud/pkg/apis/edgeclusters/v1"
	"github.com/kubeedge/kubeedge/edge/pkg/clusterd/config"
	"github.com/kubeedge/kubeedge/edge/pkg/clusterd/helper"
	"github.com/kubeedge/kubeedge/edge/pkg/clusterd/util"

	"k8s.io/klog/v2"
)

var cacheLock sync.Mutex

type MissionDeployer struct {
	MissionMatch map[string]bool
}

//NewMissionDeployer creates new mission deployer object
func NewMissionDeployer() *MissionDeployer {
	return &MissionDeployer{
		MissionMatch: map[string]bool{},
	}
}

func (m *MissionDeployer) ApplyMission(mission *edgeclustersv1.Mission) error {
	cacheLock.Lock()
	m.MissionMatch[mission.Name] = m.isMatchingMission(mission)
	cacheLock.Unlock()
	missionYaml, err := buildMissionYaml(mission)
	if err != nil {
		// log the error and move on to apply the mission content
		klog.Errorf("Error in applying mission CRD: %v. Moving on.", err)
	} else {
		deploy_mission_cmd := fmt.Sprintf("printf \"%s\" | %s apply --kubeconfig=%s -f - ", missionYaml, config.Config.KubectlCli, config.Config.Kubeconfig)
		output, err := util.ExecCommandLine(deploy_mission_cmd)
		if err != nil {
			klog.Errorf("Failed to apply mission %v: %v", mission.Name, err)
		} else {
			if strings.Contains(output, "created") {
				klog.Infof("Mission %v is created ", mission.Name)
			} else {
				klog.V(4).Infof("Mission %v is configured.", mission.Name)
			}
		}
	}

	if m.isMatchingMission(mission) == false {
		klog.V(3).Infof("Mission %v does not match this cluster, skip the content applying", mission.Name)
	} else {
		if strings.TrimSpace(mission.Spec.Content) != "" {
			deploy_content_cmd := fmt.Sprintf("printf \"%s\" | %s apply --kubeconfig=%s -f - ", mission.Spec.Content, config.Config.KubectlCli, config.Config.Kubeconfig)
			output, err := util.ExecCommandLine(deploy_content_cmd)
			if err != nil {
				klog.Errorf("Failed to apply the content of mission %v: %v", mission.Name, err)
			} else {
				if strings.Contains(output, "unchanged") {
					klog.V(4).Infof("The content of mission %v is unchanged ", mission.Name)
				} else {
					klog.Infof("The content of mission %v applied successfully ", mission.Name)
				}
			}
		}
	}

	m.UpdateState(mission, false)

	return nil
}

func (m *MissionDeployer) DeleteMission(mission *edgeclustersv1.Mission) error {
	cacheLock.Lock()
	delete(m.MissionMatch, mission.Name)
	cacheLock.Unlock()
	if m.isMatchingMission(mission) == false {
		klog.V(4).Infof("Mission %v does not match this cluster", mission.Name)
	} else {
		if strings.TrimSpace(mission.Spec.Content) != "" {
			delete_content_cmd := fmt.Sprintf("printf \"%s\" | %s delete --kubeconfig=%s -f - ", mission.Spec.Content, config.Config.KubectlCli, config.Config.Kubeconfig)
			_, err := util.ExecCommandLine(delete_content_cmd)
			if err != nil {
				klog.Errorf("Failed to revert the content of mission %v: %v", mission.Name, err)
			} else {
				klog.Infof("The content of mission %v is reverted.", mission.Name)
			}
		}
	}

	delete_mission_cmd := fmt.Sprintf("%s delete mission %s --kubeconfig=%s", config.Config.KubectlCli, mission.Name, config.Config.Kubeconfig)
	if _, err := util.ExecCommandLine(delete_mission_cmd); err != nil {
		return fmt.Errorf("Failed to delete mission %v: %v", mission.Name, err)
	}

	klog.Infof("Mission %v deleted successfully ", mission.Name)

	return nil
}

func (m *MissionDeployer) DeleteMissionByName(name string) error {
	mission, err := helper.GetMissionByName(name)
	if err != nil {
		return err
	}

	return m.DeleteMission(mission)
}

func (m *MissionDeployer) isMatchingMission(mission *edgeclustersv1.Mission) bool {
	// if the placement field is empty, it matches all the edge clusters
	if len(mission.Spec.Placement.Clusters) == 0 && len(mission.Spec.Placement.MatchLabels) == 0 {
		return true
	}

	for _, matchingCluster := range mission.Spec.Placement.Clusters {
		if config.Config.Name == matchingCluster.Name {
			return true
		}
	}

	// TODO: use k8s Labels operator to match
	if len(mission.Spec.Placement.MatchLabels) == 0 {
		return false
	}

	for k, v := range mission.Spec.Placement.MatchLabels {
		if val, ok := config.Config.Labels[k]; ok && val == v {
			return true
		}
	}

	return false
}

func (m *MissionDeployer) AlignMissionList(missionList []*edgeclustersv1.Mission) error {
	missionMap := map[string]bool{}
	var errs []error
	for _, mi := range missionList {
		missionMap[mi.Name] = true
		if err := m.ApplyMission(mi); err != nil {
			// Try to apply as many missions as possible, so move on after hitting error
			errs = append(errs, fmt.Errorf("Error when applying mission %s: %v", mi.Name, err))
		}
	}

	localMissions := helper.GetLocalClusterScopeResourceNames("missions", "")

	for _, mi := range localMissions {
		if _, exists := missionMap[mi]; !exists {
			if err := m.DeleteMissionByName(mi); err != nil {
				// Try to remove as many missions as possible, so move on after hitting error
				errs = append(errs, fmt.Errorf("Error when deleting mission %s: %v", mi, err))
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("Hit the errors in mission align: %v", errs)
}

// create a yaml to use by "kubectl apply" command
func buildMissionYaml(input *edgeclustersv1.Mission) (string, error) {

	// probably due to the json encoder in arktos, the commmnd "kubectl apply missiong" in arktos
	// fails if the mission.StateCheck.Command is nil or empty.
	// We trick it with a string with one space.
	// TODO: We need a non-hacky way.
	if input.Spec.StateCheck.Command == "" {
		input.Spec.StateCheck.Command = " "
	}

	yaml_part1_template := `apiVersion: edgeclusters.kubeedge.io/v1
kind: Mission
metadata:
  name: %s
spec:
  %s`
	specStr, err := yaml.Marshal(input.Spec)
	if err != nil {
		return "", err
	}

	output := fmt.Sprintf(yaml_part1_template, input.Name, strings.ReplaceAll(string(specStr), "\n", "\n  "))
	return output, nil
}

func (m *MissionDeployer) UpdateMissionLocalState(missionName string, stateInfo string) error {
	stateInfo = strconv.Quote(strings.TrimSpace(stateInfo))

	statePatch := fmt.Sprintf("{\"state\":{\"%s\": %s}}", LOCAL_EDGE_CLUSTER, stateInfo)

	stateUpdateCommand := fmt.Sprintf("%s patch mission %s --kubeconfig=%s --patch '%s' --type=merge", config.Config.KubectlCli, missionName, config.Config.Kubeconfig, statePatch)
	_, err := util.ExecCommandLine(stateUpdateCommand)
	if err != nil {
		if strings.Contains(err.Error(), "Error from server (NotFound):") {
			klog.V(3).Infof("Mission %v is deleted.", missionName)
			return nil
		} else {
			klog.Errorf("Error when checking the mission %v state: %v", missionName, err)
		}
	}

	return err
}

func (m *MissionDeployer) GetStateCheckCommand(mission *edgeclustersv1.Mission) string {

	command := strings.TrimSpace(mission.Spec.StateCheck.Command)
	if command != "" {
		return strings.ReplaceAll(command, "${kubectl}", config.Config.KubectlCli)
	}

	kind, name, namespace := helper.AnalyzeMissionContent(mission.Spec.Content)

	command = fmt.Sprintf("%v get %v %v -n \"%v\" --kubeconfig %v --no-headers", config.Config.KubectlCli, kind, name, namespace, config.Config.Kubeconfig)

	klog.V(3).Infof("the state check command is %v ", command)

	return command
}

// We only check the state when the mission spec changes for better efficiency
// however, if force==true, we check it even if there is no chagne.
func (m *MissionDeployer) UpdateState(mission *edgeclustersv1.Mission, force bool) {
	if !m.MissionSpecChanged(mission) && !force {
		klog.V(4).Infof("Mission %v spec is not changed, skip the state check.", mission.Name)
		return
	}

	if m.isMatchingMission(mission) == false {
		if err := m.UpdateMissionLocalState(mission.Name, STATUS_NO_MATCH); err != nil {
			klog.Errorf("Error when updating the mission %v state: %v", mission.Name, err)
		}
		return
	}

	state_command := m.GetStateCheckCommand(mission)
	output, err := util.ExecCommandLine(state_command)
	if err != nil {
		if strings.Contains(err.Error(), "Error from server (NotFound):") {
			klog.V(3).Infof("Mission %v is deleted. Return", mission.Name)
		} else {
			klog.Errorf("Error when checking the mission %v state: %v", mission.Name, err)
		}
		return
	}

	err = m.UpdateMissionLocalState(mission.Name, output)
	if err != nil {
		klog.Errorf("Error when updating the mission %v state: %v", mission.Name, err)
	}
}

// We should only update the state if there is change in the mission Spec.
// NO need to update the state if the change is in the Mission state.
// Otherwise, the system will be drained, as the clusterd will be trapped
// in getting update event which is caused by its own state update action and making another state update action.
func (m *MissionDeployer) MissionSpecChanged(mission *edgeclustersv1.Mission) bool {
	existingMission, err := helper.GetMissionByName(mission.Name)
	if err != nil {
		// "NotFound" error means it is a new mission, surely we need to check the status

		if strings.Contains(err.Error(), "Error from server (NotFound):") {
			return true
		}
		// If there are some other errors, let's just check the state
		klog.Warningf("Error in gettting mission %v : %v. Moving on. ", mission.Name, err)
		return true
	}

	if !helper.EqualMissionSpec(existingMission.Spec, mission.Spec) {
		klog.Infof("Mission %v Spec has changed. existing (%#v) new (%#v)", mission.Name, existingMission.Spec, mission.Spec)
		return true
	}

	return false
}

func (m *MissionDeployer) UnmarshalAndHandleMissionList(content []byte) (err error) {
	var lists []string
	if err = json.Unmarshal(content, &lists); err != nil {
		return err
	}

	missionList := []*edgeclustersv1.Mission{}
	for _, list := range lists {
		var mission edgeclustersv1.Mission
		err = json.Unmarshal([]byte(list), &mission)
		if err != nil {
			return err
		}
		missionList = append(missionList, &mission)
	}

	return m.AlignMissionList(missionList)
}

func (m *MissionDeployer) UnmarshalAndHandleMission(op string, content []byte) (err error) {
	var mission edgeclustersv1.Mission
	err = json.Unmarshal(content, &mission)
	if err != nil {
		return err
	}

	switch op {
	case model.InsertOperation:
		err = m.ApplyMission(&mission)
	case model.UpdateOperation:
		err = m.ApplyMission(&mission)
	case model.DeleteOperation:
		err = m.DeleteMission(&mission)
	}
	if err == nil {
		klog.V(3).Infof("%s mission [%s] succeeded.", op, mission.Name)
	}

	return err
}