package main

import (
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/lucagrulla/cw/cloudwatch"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"
)

type arrayFlags []string

type logEvent struct {
	logEvent cloudwatchlogs.FilteredLogEvent
	logGroup string
}

func (i *arrayFlags) String() string {
	return ""
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func getTaskLog(tasks []*ecs.Task, containerDefinition *ecs.ContainerDefinition) (string, *time.Time) {
	var (
		logStream string
		startTime *time.Time
	)

	for _, task := range tasks {
		startTime = task.CreatedAt
		for _, container := range task.Containers {
			if *container.Name == *containerDefinition.Name {
				taskArn := strings.Split(*container.TaskArn, "/")[1]
				logStream = fmt.Sprintf(
					"%v/%v/%v",
					*containerDefinition.LogConfiguration.Options["awslogs-stream-prefix"],
					*container.Name,
					taskArn,
				)
				return logStream, startTime
			}
		}
	}

	return logStream, startTime
}

func printLogOutput(
	describeTaskInput *ecs.DescribeTasksInput,
	ecsClient *ecs.ECS,
	logger *log.Logger,
	awsRegion *string,
	taskDefinition *ecs.DescribeTaskDefinitionOutput,
) {

	logger.Println("Waiting for task to start")
	ecsClient.WaitUntilTasksRunning(describeTaskInput)
	logger.Println("Task has started")

	discardLogger := log.New(ioutil.Discard, "you-cant-see-me", 1)
	cw := cloudwatch.New(aws.String(""), aws.String(""), awsRegion, discardLogger)

	startedTaskDescription, err := ecsClient.DescribeTasks(describeTaskInput)
	if err != nil {
		logger.Fatal(err)
	}
	var startTime *time.Time

	for _, containerDefinition := range taskDefinition.TaskDefinition.ContainerDefinitions {
		if containerDefinition.LogConfiguration == nil {
			continue
		}
		if aws.StringValue(containerDefinition.LogConfiguration.LogDriver) == "awslogs" {
			var logStream string
			logStream, startTime = getTaskLog(startedTaskDescription.Tasks, containerDefinition)
			logGroup := *containerDefinition.LogConfiguration.Options["awslogs-group"]

			endTime := time.Unix(1<<63-62135596801, 999999999)
			trigger := make(chan time.Time, 1)

			trigger <- *startTime
			go func() {
				for c := range cw.Tail(
					&logGroup,
					&logStream,
					aws.Bool(true),
					startTime,
					&endTime,
					aws.String(""),
					aws.String(""),
					trigger,
				) {
					fmt.Printf("%v: %v\n", time.Unix(*c.Timestamp/1000, 0), *c.Message)
				}
			}()
		}
	}
}

func waitForTaskStop(
	ecsClient *ecs.ECS,
	describeTaskInput *ecs.DescribeTasksInput,
	taskError chan<- error,
	taskSuccess chan<- bool,
) {
	err := ecsClient.WaitUntilTasksStopped(describeTaskInput)
	if err != nil {
		taskError <- err
		return
	}
	stoppedTaskDescription, err := ecsClient.DescribeTasks(describeTaskInput)
	if err != nil {
		taskError <- err
		return
	}

	for _, task := range stoppedTaskDescription.Tasks {
		for _, container := range task.Containers {
			if container.ExitCode == nil {
				if container.Reason != nil {
					taskError <- fmt.Errorf(*container.Reason)
					return
				}
				if task.StoppedReason != nil {
					taskError <- fmt.Errorf(*task.StoppedReason)
					return
				}
			} else if int(*container.ExitCode) != 0 {
				taskError <- fmt.Errorf("Container: %v, Exit Code: %v", *container.Name, *container.ExitCode)
				return
			}
		}
	}
	taskSuccess <- true
}

func main() {
	logger := log.New(os.Stderr, "ecs-task-runner ", 1)

	var (
		containerCommandOverrides arrayFlags
		containerEnvOverrides     arrayFlags
		securityGroups            arrayFlags
		vpcSubnets                arrayFlags
	)

	flag.Usage = func() {
		fmt.Fprintf(
			flag.CommandLine.Output(),
			`Run a AWS ECS task and stream the Cloudwatch logs output

Usage:
  ecs-task-runner [flags]

Examples:
  ecs-task-runner -security-group sg-abcabcabcd -subnet subnet-1231231231 -task-definition task-def-name
  ecs-task-runner -env-host -cluster example -command=container-name='echo hello' -security-group sg-abcabcabcd -subnet subnet-1231231231 -task-definition task-def-name:2
  ecs-task-runner -env-host -env=container-name=HOME=/root -env=container-name=SHELL=/bin/bash -security-group sg-abcabcabcd -subnet subnet-1231231231 -task-definition task-def-name:2

Flags:
`,
		)
		flag.PrintDefaults()
	}

	taskDefinitionName := flag.String("task-definition", "", "The task definition family name with or without family version.")
	ecsCluster := flag.String("cluster", "default", "Name of the ECS cluster to use.")
	awsRegion := flag.String("region", "us-west-2", "AWS region.")
	ecsFargate := flag.Bool("fargate", true, "Use AWS ECS fargate.")
	envHost := flag.Bool("env-host", false, "Use all current host environment variables in container.")
	flag.Var(&containerCommandOverrides, "command", "Override command for a container. May be specified multiple times.  Format: -command 'containerName=\"echo \\'hello\\'\"'")
	flag.Var(&containerEnvOverrides, "env", "Override environment variables for a container. May be specified multiple times.  Format: -env 'containerName=VARIABLE=VALUE'")
	flag.Var(&securityGroups, "security-group", "A security group to assign the to task. May be specified multiple times.")
	flag.Var(&vpcSubnets, "subnet", "A VPC subnet for the task.  May be specifid multiple times.")
	flag.Parse()

	ecsClient := ecs.New(session.New(&aws.Config{
		Region: aws.String(*awsRegion),
	}))

	taskDefinitionInput := &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(*taskDefinitionName),
	}
	taskDefinition, err := ecsClient.DescribeTaskDefinition(taskDefinitionInput)
	if err != nil {
		logger.Fatal(err)

	}

	launchType := "FARGATE"
	if *ecsFargate == false {
		launchType = "EC2"
	}

	taskInput := &ecs.RunTaskInput{
		Cluster:        ecsCluster,
		TaskDefinition: taskDefinitionName,
		LaunchType:     aws.String(launchType),
	}

	if aws.StringValue(taskDefinition.TaskDefinition.NetworkMode) == "awsvpc" {
		taskInput.SetNetworkConfiguration(&ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				SecurityGroups: aws.StringSlice([]string(securityGroups)),
				Subnets:        aws.StringSlice([]string(vpcSubnets)),
			},
		})
	}

	commmandOverrideMap := make(map[string][]*string)
	if len(containerCommandOverrides) > 0 {
		for _, containerOverride := range containerCommandOverrides {
			containerOverrideSplit := strings.SplitN(containerOverride, "=", 2)
			containerOverrideCommand := strings.Split(containerOverrideSplit[1], " ")
			if len(containerOverrideSplit) != 2 {
				logger.Fatal(fmt.Errorf("Container override must be in format containerName=Command.  Found %v", containerOverride))
			}
			commmandOverrideMap[containerOverrideSplit[0]] = aws.StringSlice(containerOverrideCommand)
		}
	}

	envOverrideMap := make(map[string][]*ecs.KeyValuePair)
	if len(containerEnvOverrides) > 0 {
		for _, envOverride := range containerEnvOverrides {
			envOverrideSplit := strings.SplitN(envOverride, "=", 3)
			if len(envOverrideSplit) != 3 {
				logger.Fatal(fmt.Errorf("Environment override must be in format containerName=KEY=VALUE.  Found %v", envOverride))
			}
			envValuePair := ecs.KeyValuePair{
				Name:  &envOverrideSplit[1],
				Value: &envOverrideSplit[2],
			}
			envOverrideMap[envOverrideSplit[0]] = append(envOverrideMap[envOverrideSplit[0]], &envValuePair)
		}
	}

	if *envHost == true {
		// Get all env variables
		for _, containerDefinition := range taskDefinition.TaskDefinition.ContainerDefinitions {
			for _, envPair := range os.Environ() {
				// for each environment variable add to envOverrideMap
				envSplit := strings.SplitN(envPair, "=", 2)
				envValuePair := ecs.KeyValuePair{
					Name:  &envSplit[0],
					Value: &envSplit[1],
				}
				envOverrideMap[*containerDefinition.Name] = append(envOverrideMap[*containerDefinition.Name], &envValuePair)
			}
		}
	}

	var taskOverride ecs.TaskOverride

	// loop over all containers and add a ecs.ContainerOverride.
	var containerOverrides []*ecs.ContainerOverride
	for _, containerDefinition := range taskDefinition.TaskDefinition.ContainerDefinitions {
		var containerOverride ecs.ContainerOverride
		containerName := *containerDefinition.Name
		containerOverride.SetName(containerName)

		// Command overrides
		if command, ok := commmandOverrideMap[containerName]; ok {
			containerOverride.SetCommand(command)
		}

		// Environment overrides
		if env, ok := envOverrideMap[containerName]; ok {
			containerOverride.SetEnvironment(env)
		}

		containerOverrides = append(containerOverrides, &containerOverride)

	}

	taskOverride.ContainerOverrides = containerOverrides
	taskInput.SetOverrides(&taskOverride)

	runningTasks, err := ecsClient.RunTask(taskInput)
	if err != nil {
		logger.Fatal(err)
	}

	logger.Print(runningTasks)
	var runningTaskArns []string
	for _, task := range runningTasks.Tasks {
		runningTaskArns = append(runningTaskArns, *task.TaskArn)
	}

	describeTaskInput := &ecs.DescribeTasksInput{
		Cluster: ecsCluster,
		Tasks:   aws.StringSlice(runningTaskArns),
	}

	taskError := make(chan error)
	taskSuccess := make(chan bool)

	go waitForTaskStop(ecsClient, describeTaskInput, taskError, taskSuccess)

	printLogOutput(describeTaskInput, ecsClient, logger, awsRegion, taskDefinition)
	// Wait for any final logs
	time.Sleep(3 * time.Second)

	select {
	case <-taskSuccess:
		log.Println("Task stopped succesffully")
	case err := <-taskError:
		log.Println("Error: ", err)
		defer os.Exit(1)
	}
}
