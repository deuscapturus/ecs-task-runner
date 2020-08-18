package main

import "flag"
import "fmt"
import "log"
import "github.com/aws/aws-sdk-go/service/ecs"
import "github.com/aws/aws-sdk-go/aws/session"

//import "github.com/aws/aws-sdk-go/service/cloudwatchlogs"
import "github.com/aws/aws-sdk-go/aws"

//import "sync"
import "os"
import "time"

type arrayFlags []string

func (i *arrayFlags) String() string {
	return ""
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func main() {

	log.SetOutput(os.Stderr)

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
		log.Fatal(err)

	}

	var logGroups []string
	for _, container := range taskDefinition.TaskDefinition.ContainerDefinitions {
		if container.LogConfiguration == nil {
			continue
		}
		if aws.StringValue(container.LogConfiguration.LogDriver) == "awslogs" {
			logGroups = append(logGroups, *container.LogConfiguration.Options["awslogs-group"])
		}

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

	runningTasks, err := ecsClient.RunTask(taskInput)
	if err != nil {
		log.Fatal(err)
	}

	log.Print(runningTasks)
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
	go func() {

		err = ecsClient.WaitUntilTasksStopped(describeTaskInput)
		if err != nil {
			TaskError <- err
		}
		stoppedTaskDescription, err := ecsClient.DescribeTasks(describeTaskInput)
		if err != nil {
			TaskError <- err
		}

		for _, task := range stoppedTaskDescription.Tasks {
			for _, container := range task.Containers {
				log.Println(container)
				if container.ExitCode == nil {
					TaskError <- fmt.Errorf(*container.Reason)
				} else if int(*container.ExitCode) != 0 {
					TaskError <- fmt.Errorf("%q\n%q\n\n%q\n%q", container.ContainerArn, container.Image, container.LastStatus, *container.Reason)
				}
			}
		}
	}()

	// Log printing goes here
	go func() {

		log.Println("Waiting for task to start")
		// Placeholder for cloudwatchlogs stream.  TODO
		//ecsClient.WaitUntilTasksRunning(describeTaskInput)
		for i := 1; true; i++ {
			fmt.Println(i)
			time.Sleep(time.Second)
		}
	}()

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
