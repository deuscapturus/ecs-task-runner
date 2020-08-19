package main

import "flag"
import "fmt"
import "log"
import "github.com/aws/aws-sdk-go/service/ecs"
import "github.com/aws/aws-sdk-go/service/cloudwatchlogs"
import "github.com/aws/aws-sdk-go/aws/session"
import "github.com/lucagrulla/cw/cloudwatch"
import "github.com/aws/aws-sdk-go/aws"
import "os"
import "time"
import "io/ioutil"
import "strings"

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

func printLogOutput(describeTaskInput *ecs.DescribeTasksInput, ecsClient *ecs.ECS, logger *log.Logger, awsRegion *string, taskDefinition *ecs.DescribeTaskDefinitionOutput) {

	logger.Println("Waiting for task to start")
	ecsClient.WaitUntilTasksRunning(describeTaskInput)
	logger.Println("Task has started")

	discardLogger := log.New(ioutil.Discard, "you-cant-see-me", 1)
	cw := cloudwatch.New(aws.String(""), aws.String(""), awsRegion, discardLogger)

	startedTaskDescription, _ := ecsClient.DescribeTasks(describeTaskInput)
	var startTime *time.Time

	for _, containerDefinition := range taskDefinition.TaskDefinition.ContainerDefinitions {
		if containerDefinition.LogConfiguration == nil {
			continue
		}
		if aws.StringValue(containerDefinition.LogConfiguration.LogDriver) == "awslogs" {
			var logStream string
			for _, task := range startedTaskDescription.Tasks {
				startTime = task.StartedAt
				for _, container := range task.Containers {
					if *container.Name == *containerDefinition.Name {
						taskArn := strings.Split(*container.TaskArn, "/")[1]
						logStream = fmt.Sprintf(
							"%v/%v/%v",
							*containerDefinition.LogConfiguration.Options["awslogs-stream-prefix"],
							*container.Name,
							taskArn,
						)
						break
					}
				}
			}
			logGroup := *containerDefinition.LogConfiguration.Options["awslogs-group"]

			// beginning of all time
			// end of all time
			endTime := time.Unix(1<<63-62135596801, 999999999)
			trigger := make(chan time.Time, 1)

			trigger <- *startTime
			go func() {
				for c := range cw.Tail(&logGroup, &logStream, aws.Bool(true), startTime, &endTime, aws.String(""), aws.String(""), trigger) {
					fmt.Printf("%v: %v", *c.Timestamp, *c.Message)
				}
			}()

		}
	}

}

func main() {

	logger := log.New(os.Stderr, "ecs-task-runner ", 1)

	var containerCommandOverrides arrayFlags
	var securityGroups arrayFlags
	var vpcSubnets arrayFlags

	taskDefinitionName := flag.String("task-definition", "", "The task definition family name with or without family version.")
	ecsCluster := flag.String("cluster", "default", "Name of the ECS cluster to use.")
	awsRegion := flag.String("region", "us-west-2", "AWS region.")
	ecsFargate := flag.Bool("fargate", true, "Use AWS ECS fargate.")
	flag.Var(&containerCommandOverrides, "container", "Override command for a container. May be specified multiple times.  Format: -container 'name=\"echo \\'hello\\'\"'")
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

	if len(containerCommandOverrides) > 0 {

		var containerOverrides []*ecs.ContainerOverride
		for _, containerOverride := range containerCommandOverrides {
			containerOverrideMap := strings.Split(containerOverride, "=")
			if len(containerOverrideMap) != 2 {
				logger.Fatal(fmt.Errorf("Container override must be in format containerName=Command.  Found %v", containerOverride))
			}

			containerOverrides = append(containerOverrides, &ecs.ContainerOverride{
				Name:    &containerOverrideMap[0],
				Command: []*string{&containerOverrideMap[1]},
			})
		}
		taskOverrides := &ecs.TaskOverride{ContainerOverrides: containerOverrides}
		taskInput.SetOverrides(taskOverrides)
	}

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

	TaskError := make(chan error)
	TaskSuccess := make(chan bool)

	// Wait for task to stop
	go func(TaskError chan<- error, TaskSuccess chan<- bool) {

		err = ecsClient.WaitUntilTasksStopped(describeTaskInput)
		if err != nil {
			TaskError <- err
			return
		}
		stoppedTaskDescription, err := ecsClient.DescribeTasks(describeTaskInput)
		if err != nil {
			TaskError <- err
			return
		}

		for _, task := range stoppedTaskDescription.Tasks {
			for _, container := range task.Containers {
				if container.ExitCode == nil {
					TaskError <- fmt.Errorf(*container.Reason)
					return
				} else if int(*container.ExitCode) != 0 {
					TaskError <- fmt.Errorf("%q\n%q\n\n%q\n%q", container.ContainerArn, container.Image, container.LastStatus, *container.Reason)
					return
				}
			}
		}
		TaskSuccess <- true
	}(TaskError, TaskSuccess)

	printLogOutput(describeTaskInput, ecsClient, logger, awsRegion, taskDefinition)
	// Wait for any final logs
	time.Sleep(3 * time.Second)

	// Wait for signal from TaskSuccess or TaskError channels
	select {
	case <-TaskSuccess:
		log.Println("Task stopped succesffully")
	case err := <-TaskError:
		log.Println("Error: ", err)
		defer os.Exit(1)
	}

	return
}
