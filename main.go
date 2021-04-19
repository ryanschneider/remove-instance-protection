package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/hashicorp/logutils"
	flags "github.com/jessevdk/go-flags"
	"github.com/meirf/gopart"
	"github.com/pkg/errors"
)

// Options contains the flag options
type Options struct {
	LogLevel              string `long:"log-level" description:"The minimum log level to output (DEBUG, INFO, WARN, ERROR, FATAL)" default:"INFO"`
	ASG                   string `long:"asg" description:"The ASG to update." required:"true"`
	DryRun                bool   `long:"dry-run" description:"If set updates are not actually performed."`
	Version               bool   `long:"version" description:"print version and exit"`
	Force                 bool   `long:"force" description:"by default if no instances are found at latest version tool does nothing"`
	PrintLatestInstances  bool   `long:"output-latest-instances" description:"print up-to-date instances to stdout"`
	PrintInvalidInstances bool   `long:"output-invalid-instances" description:"print out-of-date instances to stdout"`
	Deregister            bool   `long:"deregister-from-target-groups" description:"remove old instances from target groups as well"`
}

// These variables are filled by goreleaser
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	options := Options{}
	parser := flags.NewParser(&options, flags.Default)
	_, err := parser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); ok && e.Type != flags.ErrHelp {
			fmt.Printf("\n")
			parser.WriteHelp(os.Stderr)
			fmt.Printf("\n")
		}
		os.Exit(1)
	}

	// Init Logger
	filter := &logutils.LevelFilter{
		Levels:   []logutils.LogLevel{"SPAM", "DEBUG", "INFO", "WARN", "ERROR", "DRYRUN"},
		MinLevel: logutils.LogLevel(options.LogLevel),
		Writer:   os.Stderr,
	}
	log.SetOutput(filter)

	if options.Version {
		fmt.Printf("%s-%s-%s\n", version, commit, date)
		os.Exit(0)
	}

	err = doUpdate(&options)
	if err != nil {
		log.Fatalf("[FATAL] error updating: %v", err)
	}
}

func doUpdate(options *Options) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	asgClient := autoscaling.New(sess)
	albClient := elbv2.New(sess)

	log.Printf("[DEBUG] describing ASG %s...", options.ASG)
	asgResponse, err := asgClient.DescribeAutoScalingGroups(&autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{
			aws.String(options.ASG),
		},
	})
	if err != nil {
		return errors.Wrap(err, "could not describe Auto Scaling Group")
	}
	if asgResponse == nil {
		return errors.New("invalid describe Auto Scaling Group response")
	}
	if len(asgResponse.AutoScalingGroups) != 1 {
		return errors.Errorf("auto scaling group \"%s\" not found", options.ASG)
	}

	asg := asgResponse.AutoScalingGroups[0]
	var ltName *string
	if asg.LaunchTemplate != nil {
		ltName = asg.LaunchTemplate.LaunchTemplateName
	} else if asg.MixedInstancesPolicy != nil && asg.MixedInstancesPolicy.LaunchTemplate != nil {
		ltName = asg.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateName
	}
	if ltName == nil {
		return errors.Errorf("auto scaling group \"%s\" does not use Launch Templates", options.ASG)
	}

	log.Printf("[DEBUG] ASG %s uses Launch Template %s, describing LT...", options.ASG, *ltName)
	ec2Client := ec2.New(sess)
	ltResponse, err := ec2Client.DescribeLaunchTemplates(&ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []*string{
			ltName,
		},
	})
	if err != nil {
		return errors.Wrap(err, "could not describe Launch Template "+*ltName)
	}
	if ltResponse == nil || len(ltResponse.LaunchTemplates) != 1 {
		return errors.New("invalid describe Launch Template response for " + *ltName)
	}

	lt := ltResponse.LaunchTemplates[0]
	if lt.LatestVersionNumber == nil {
		return errors.New("no latest version for Launch Template " + *ltName)
	}
	latestVersion := *lt.LatestVersionNumber
	log.Printf("[INFO] ASG %s has latest version %d, looking for old instances...", options.ASG, latestVersion)
	instanceIdsToRemove := make([]*string, 0)
	latestInstances := make([]string, 0)
	invalidInstances := make([]string, 0)
	oldInstances := make([]*string, 0)

	for _, instance := range asg.Instances {
		if instance.LaunchTemplate == nil || instance.LaunchTemplate.Version == nil {
			return errors.New("missing Launch Template version for instance id " + *instance.InstanceId)
		}
		if *instance.LaunchTemplate.LaunchTemplateName != *ltName {
			log.Printf(
				"[WARN] instance %s has different Launch Template than ASG: %s:%s",
				*instance.InstanceId,
				*instance.LaunchTemplate.LaunchTemplateName,
				*instance.LaunchTemplate.Version,
			)
			if *instance.ProtectedFromScaleIn == false {
				log.Printf("[DEBUG] instance %s is already not protected from scale-in, skipping", *instance.InstanceId)
				oldInstances = append(oldInstances, instance.InstanceId)
			} else {
				instanceIdsToRemove = append(instanceIdsToRemove, instance.InstanceId)
			}
			continue
		}

		version, err := strconv.ParseInt(*instance.LaunchTemplate.Version, 10, 64)
		if err != nil {
			return errors.Wrap(err, "invalid instance Launch Template Version")
		}

		if version != latestVersion {
			log.Printf("[DEBUG] instance %s has old version %d", *instance.InstanceId, version)
			invalidInstances = append(invalidInstances, *instance.InstanceId)
			if *instance.ProtectedFromScaleIn == false {
				log.Printf("[DEBUG] old instance %s is already not protected from scale-in, skipping", *instance.InstanceId)
				oldInstances = append(oldInstances, instance.InstanceId)
			} else {
				instanceIdsToRemove = append(instanceIdsToRemove, instance.InstanceId)
			}
		} else {
			latestInstances = append(latestInstances, *instance.InstanceId)
		}
	}

	if options.PrintLatestInstances {
		for _, instance := range latestInstances {
			fmt.Println(instance)
		}
	}
	if options.PrintInvalidInstances {
		for _, instance := range invalidInstances {
			fmt.Println(instance)
		}
	}

	if options.Deregister && len(latestInstances) > 0 && len(oldInstances) > 0 {
		// find target groups to remove instances from
		for _, tg := range asg.TargetGroupARNs {
			healthy, err := albClient.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
				TargetGroupArn: tg,
			})
			if err != nil {
				return errors.Wrapf(err, "could not get target group instances for %s", *tg)
			}

			targets := make([]*elbv2.TargetDescription, 0)
			for _, h := range healthy.TargetHealthDescriptions {
				for _, old := range oldInstances {
					if *h.Target.Id == *old {
						targets = append(targets, h.Target)
					}
				}
			}

			if options.DryRun {
				for _, target := range targets {
					log.Printf("[DRYRUN] would remove instance %s from target group %s", strings.ReplaceAll(target.String(), "\n", ""), *tg)
				}
			} else {
				_, err = albClient.DeregisterTargets(&elbv2.DeregisterTargetsInput{
					TargetGroupArn: tg,
					Targets:        targets,
				})
				if err != nil {
					return errors.Wrapf(err, "could not deregister targets from %s", *tg)
				}
				log.Printf("[INFO] Removed %d instances from %s", len(targets), *tg)
			}
		}
	}

	if len(instanceIdsToRemove) == 0 {
		log.Printf("[INFO] No old instances with scale in protection enabled found")
		return nil
	}

	if len(latestInstances) == 0 {
		log.Printf("[WARN] No instances at latest Launch Template version %d found", latestVersion)
		if !options.Force {
			log.Printf("[WARN] no changes made, use `--force` flag to override this behavior")
			return nil
		} else {
			log.Printf("[WARN] `--force` flag provided, potentially updating all instances")
		}
	}

	if options.DryRun {
		log.Printf("[DRYRUN] Removing scale in protection for %d instances", len(instanceIdsToRemove))
	} else {
		log.Printf("[INFO] Removing scale in protection for %d instances", len(instanceIdsToRemove))
	}

	// partition into groups of at most 50
	for partition := range gopart.Partition(len(instanceIdsToRemove), 50) {
		instanceIds := instanceIdsToRemove[partition.Low:partition.High]
		if options.DryRun {
			for _, instance := range instanceIds {
				log.Printf("[DRYRUN] would remove instance protection on instanceId %s", *instance)
			}
			continue
		}

		log.Printf("[DEBUG] calling SetInstanceProtection with %d instances", len(instanceIds))
		_, err = asgClient.SetInstanceProtection(&autoscaling.SetInstanceProtectionInput{
			AutoScalingGroupName: aws.String(options.ASG),
			InstanceIds:          instanceIdsToRemove,
			ProtectedFromScaleIn: aws.Bool(false),
		})
		if err != nil {
			return errors.Wrap(err, "set instance protection failed")
		}

		for _, instance := range instanceIds {
			log.Printf("[DEBUG] instance protection removed for instance: %s", *instance)
		}
	}
	return nil
}
