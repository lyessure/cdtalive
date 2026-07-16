package aliyun

import (
	"encoding/json"
	"fmt"
	"strconv"

	"cdtalive/internal/config"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
)

type Instance struct {
	ID        string
	Status    string
	StartTime string
}

type Client struct {
	common *sdk.Client
	ecs    *ecs.Client
}

func New(cfg config.Config) (*Client, error) {
	common, err := sdk.NewClientWithAccessKey(cfg.RegionID, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, err
	}
	ecsClient, err := ecs.NewClientWithAccessKey(cfg.RegionID, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, err
	}
	return &Client{common: common, ecs: ecsClient}, nil
}

func (c *Client) TrafficGB() (float64, error) {
	request := requests.NewCommonRequest()
	request.Method = requests.POST
	request.Scheme = "https"
	request.Domain = "cdt.aliyuncs.com"
	request.Version = "2021-08-13"
	request.ApiName = "ListCdtInternetTraffic"
	response, err := c.common.ProcessCommonRequest(request)
	if err != nil {
		return 0, err
	}
	var payload struct {
		TrafficDetails []struct {
			Traffic json.RawMessage `json:"Traffic"`
		} `json:"TrafficDetails"`
	}
	if err := json.Unmarshal(response.GetHttpContentBytes(), &payload); err != nil {
		return 0, err
	}
	var bytes float64
	for _, detail := range payload.TrafficDetails {
		value, err := number(detail.Traffic)
		if err != nil {
			return 0, fmt.Errorf("解析 CDT 流量失败: %w", err)
		}
		bytes += value
	}
	return bytes / (1024 * 1024 * 1024), nil
}

func (c *Client) Balance() (float64, error) {
	request := requests.NewCommonRequest()
	request.Method = requests.POST
	request.Scheme = "https"
	request.Domain = "business.aliyuncs.com"
	request.Version = "2017-12-14"
	request.ApiName = "QueryAccountBalance"
	response, err := c.common.ProcessCommonRequest(request)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Data struct {
			AvailableAmount json.RawMessage `json:"AvailableAmount"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(response.GetHttpContentBytes(), &payload); err != nil {
		return 0, err
	}
	if len(payload.Data.AvailableAmount) == 0 || string(payload.Data.AvailableAmount) == "null" {
		return 0, nil
	}
	return number(payload.Data.AvailableAmount)
}

func (c *Client) Instances() ([]Instance, error) {
	request := ecs.CreateDescribeInstancesRequest()
	request.Scheme = "https"
	request.PageSize = requests.NewInteger(100)
	response, err := c.ecs.DescribeInstances(request)
	if err != nil {
		return nil, err
	}
	instances := make([]Instance, 0, len(response.Instances.Instance))
	for _, item := range response.Instances.Instance {
		instances = append(instances, Instance{ID: item.InstanceId, Status: item.Status, StartTime: item.StartTime})
	}
	return instances, nil
}

func (c *Client) Instance(instanceID string) (*Instance, error) {
	request := ecs.CreateDescribeInstancesRequest()
	request.Scheme = "https"
	request.InstanceIds = fmt.Sprintf("[%q]", instanceID)
	response, err := c.ecs.DescribeInstances(request)
	if err != nil {
		return nil, err
	}
	if len(response.Instances.Instance) == 0 {
		return nil, nil
	}
	item := response.Instances.Instance[0]
	return &Instance{ID: item.InstanceId, Status: item.Status, StartTime: item.StartTime}, nil
}

func (c *Client) Start(instanceID string) error {
	request := ecs.CreateStartInstancesRequest()
	request.Scheme = "https"
	request.InstanceId = &[]string{instanceID}
	_, err := c.ecs.StartInstances(request)
	return err
}

func (c *Client) Stop(instanceID string) error {
	request := ecs.CreateStopInstancesRequest()
	request.Scheme = "https"
	request.InstanceId = &[]string{instanceID}
	request.ForceStop = requests.NewBoolean(false)
	request.StoppedMode = "StopCharging"
	_, err := c.ecs.StopInstances(request)
	return err
}

func number(raw json.RawMessage) (float64, error) {
	var numeric float64
	if err := json.Unmarshal(raw, &numeric); err == nil {
		return numeric, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, err
	}
	if text == "" {
		return 0, nil
	}
	return strconv.ParseFloat(text, 64)
}
