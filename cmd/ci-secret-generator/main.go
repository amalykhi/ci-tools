package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/getlantern/deepcopy"
	"github.com/sirupsen/logrus"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/logrusutil"

	"github.com/openshift/ci-tools/pkg/api/secretbootstrap"
	"github.com/openshift/ci-tools/pkg/bitwarden"
)

type options struct {
	logLevel            string
	configPath          string
	bootstrapConfigPath string
	bwUser              string
	dryRun              bool
	validate            bool
	validateOnly        bool
	bwPasswordPath      string
	maxConcurrency      int

	config          []bitWardenItem
	bootstrapConfig secretbootstrap.Config
	bwPassword      string
}

type bitWardenItem struct {
	ItemName    string              `json:"item_name"`
	Fields      []fieldGenerator    `json:"fields,omitempty"`
	Attachments []fieldGenerator    `json:"attachments,omitempty"`
	Password    string              `json:"password,omitempty"`
	Notes       string              `json:"notes"`
	Params      map[string][]string `json:"params,omitempty"`
}

type fieldGenerator struct {
	Name string `json:"name,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
}

func parseOptions() options {
	var o options
	flag.CommandLine.BoolVar(&o.dryRun, "dry-run", true, "Whether to actually create the secrets with bw command")
	flag.CommandLine.StringVar(&o.configPath, "config", "", "Path to the config file to use for this tool.")
	flag.CommandLine.StringVar(&o.bootstrapConfigPath, "bootstrap-config", "", "Path to the config file used for bootstrapping cluster secrets after using this tool.")
	flag.CommandLine.BoolVar(&o.validate, "validate", true, "Validate that the items created from this tool are used in bootstrapping")
	flag.CommandLine.BoolVar(&o.validateOnly, "validate-only", false, "If the tool should exit after the validation")
	flag.CommandLine.StringVar(&o.bwUser, "bw-user", "", "Username to access BitWarden.")
	flag.CommandLine.StringVar(&o.bwPasswordPath, "bw-password-path", "", "Path to a password file to access BitWarden.")
	flag.CommandLine.StringVar(&o.logLevel, "log-level", "info", fmt.Sprintf("Log level is one of %v.", logrus.AllLevels))
	flag.CommandLine.IntVar(&o.maxConcurrency, "concurrency", 1, "Maximum number of concurrent in-flight goroutines to BitWarden.")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Errorf("cannot parse args: %q", os.Args[1:])
	}
	return o
}

func (o *options) validateOptions() error {
	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid log level specified: %w", err)
	}
	logrus.SetLevel(level)
	if o.bwUser == "" {
		return errors.New("--bw-user is empty")
	}
	if o.bwPasswordPath == "" {
		return errors.New("--bw-password-path is empty")
	}
	if o.configPath == "" {
		return errors.New("--config is empty")
	}
	if o.validate && o.bootstrapConfigPath == "" {
		return errors.New("--bootstrap-config is required with --validate")
	}
	return nil
}

func (o *options) completeOptions(secrets sets.String) error {
	bytes, err := ioutil.ReadFile(o.bwPasswordPath)
	if err != nil {
		return err
	}
	o.bwPassword = strings.TrimSpace(string(bytes))
	secrets.Insert(o.bwPassword)

	bytes, err = ioutil.ReadFile(o.configPath)
	if err != nil {
		return err
	}

	err = yaml.Unmarshal(bytes, &o.config)
	if err != nil {
		return err
	}

	if o.bootstrapConfigPath != "" {
		bytes, err = ioutil.ReadFile(o.bootstrapConfigPath)
		if err != nil {
			return err
		}

		err = yaml.Unmarshal(bytes, &o.bootstrapConfig)
		if err != nil {
			return err
		}
	}
	return o.validateCompletedOptions()
}

func cmdEmptyErr(itemIndex, entryIndex int, entry string) error {
	return fmt.Errorf("config[%d].%s[%d]: empty field not allowed for cmd if name is specified", itemIndex, entry, entryIndex)
}

func (o *options) validateCompletedOptions() error {
	if o.bwPassword == "" {
		return fmt.Errorf("--bw-password-path was empty")
	}

	for i, bwItem := range o.config {
		if bwItem.ItemName == "" {
			return fmt.Errorf("config[%d].itemName: empty key is not allowed", i)
		}

		for fieldIndex, field := range bwItem.Fields {
			if field.Name != "" && field.Cmd == "" {
				return cmdEmptyErr(i, fieldIndex, "fields")
			}
		}
		for attachmentIndex, attachment := range bwItem.Fields {
			if attachment.Name != "" && attachment.Cmd == "" {
				return cmdEmptyErr(i, attachmentIndex, "attachments")
			}
		}
		for paramName, params := range bwItem.Params {
			if len(params) == 0 {
				return fmt.Errorf("at least one argument required for param: %s, itemName: %s", paramName, bwItem.ItemName)
			}
		}
	}
	return nil
}

func executeCommand(command string) ([]byte, error) {
	out, err := exec.Command("bash", "-o", "errexit", "-o", "nounset", "-o", "pipefail", "-c", command).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s : %w", string(out), err)
	}
	return out, nil
}

func replaceParameter(paramName, param, template string) string {
	return strings.ReplaceAll(template, fmt.Sprintf("$(%s)", paramName), param)
}

func processBwParameters(bwItems []bitWardenItem) ([]bitWardenItem, error) {
	var errs []error
	processedBwItems := []bitWardenItem{}
	for _, bwItemWithParams := range bwItems {
		hasErrors := false
		bwItemsProcessingHolder := []bitWardenItem{bwItemWithParams}
		for paramName, params := range bwItemWithParams.Params {
			bwItemsProcessed := []bitWardenItem{}
			for _, qItem := range bwItemsProcessingHolder {
				for _, param := range params {
					argItem := bitWardenItem{}
					err := deepcopy.Copy(&argItem, &qItem)
					if err != nil {
						errs = append(errs, fmt.Errorf("error copying bitWardenItem %v: %w", bwItemWithParams, err))
					}
					argItem.ItemName = replaceParameter(paramName, param, argItem.ItemName)
					for i, field := range argItem.Fields {
						argItem.Fields[i].Name = replaceParameter(paramName, param, field.Name)
						argItem.Fields[i].Cmd = replaceParameter(paramName, param, field.Cmd)
					}
					for i, attachment := range argItem.Attachments {
						argItem.Attachments[i].Name = replaceParameter(paramName, param, attachment.Name)
						argItem.Attachments[i].Cmd = replaceParameter(paramName, param, attachment.Cmd)
					}
					argItem.Password = replaceParameter(paramName, param, argItem.Password)
					argItem.Notes = replaceParameter(paramName, param, argItem.Notes)
					bwItemsProcessed = append(bwItemsProcessed, argItem)
				}
			}
			bwItemsProcessingHolder = bwItemsProcessed
		}
		if !hasErrors {
			processedBwItems = append(processedBwItems, bwItemsProcessingHolder...)
		}
	}
	return processedBwItems, utilerrors.NewAggregate(errs)
}

func updateSecrets(bwItems []bitWardenItem, bwClient bitwarden.Client) error {
	var errs []error
	for _, bwItem := range bwItems {
		logger := logrus.WithField("item", bwItem.ItemName)
		for _, field := range bwItem.Fields {
			logger = logger.WithFields(logrus.Fields{
				"field":   field.Name,
				"command": field.Cmd,
			})
			logger.Info("processing field")
			out, err := executeCommand(field.Cmd)
			if err != nil {
				msg := "failed to generate field"
				logger.WithError(err).Error(msg)
				errs = append(errs, errors.New(msg))
				continue
			}
			if err := bwClient.SetFieldOnItem(bwItem.ItemName, field.Name, out); err != nil {
				msg := "failed to upload field"
				logger.WithError(err).Error(msg)
				errs = append(errs, errors.New(msg))
				continue
			}
		}
		for _, attachment := range bwItem.Attachments {
			logger = logger.WithFields(logrus.Fields{
				"attachment": attachment.Name,
				"command":    attachment.Cmd,
			})
			logger.Info("processing attachment")
			out, err := executeCommand(attachment.Cmd)
			if err != nil {
				msg := "failed to generate attachment"
				logger.WithError(err).Error(msg)
				errs = append(errs, errors.New(msg))
				continue
			}
			if err := bwClient.SetAttachmentOnItem(bwItem.ItemName, attachment.Name, out); err != nil {
				msg := "failed to upload attachment"
				logger.WithError(err).Error(msg)
				errs = append(errs, errors.New(msg))
				continue
			}
		}
		if bwItem.Password != "" {
			logger = logger.WithFields(logrus.Fields{
				"password": bwItem.Password,
			})
			logger.Info("processing password")
			out, err := executeCommand(bwItem.Password)
			if err != nil {
				msg := "failed to generate password"
				logger.WithError(err).Error(msg)
				errs = append(errs, errors.New(msg))
			} else {
				if err := bwClient.SetPassword(bwItem.ItemName, out); err != nil {
					msg := "failed to upload password"
					logger.WithError(err).Error(msg)
					errs = append(errs, errors.New(msg))
				}
			}
		}

		// Adding the notes not empty check here since we dont want to overwrite any notes that might already be present
		// If notes have to be deleted, it would have to be a manual operation where the user goes to the bw web UI and removes
		// the notes
		if bwItem.Notes != "" {
			logger = logger.WithFields(logrus.Fields{
				"notes": bwItem.Notes,
			})
			logger.Info("adding notes")
			if err := bwClient.UpdateNotesOnItem(bwItem.ItemName, bwItem.Notes); err != nil {
				msg := "failed to update notes"
				logger.WithError(err).Error(msg)
				errs = append(errs, errors.New(msg))
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func main() {
	// CLI tool which does the secret generation and uploading to bitwarden
	o := parseOptions()
	secrets := sets.NewString()
	logrus.SetFormatter(logrusutil.NewCensoringFormatter(logrus.StandardLogger().Formatter, func() sets.String {
		return secrets
	}))
	if err := o.validateOptions(); err != nil {
		logrus.WithError(err).Fatal("invalid arguments.")
	}
	if err := o.completeOptions(secrets); err != nil {
		logrus.WithError(err).Fatal("failed to complete options.")
	}
	processedBwItems, err := processBwParameters(o.config)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse parameters.")
	}

	bitWardenContexts := bitwardenContextsFor(processedBwItems)
	if err := validateContexts(bitWardenContexts, o.bootstrapConfig); err != nil {
		if o.validate {
			logrus.WithError(err).Fatal("Failed to validate secret entries.")
		} else {
			logrus.WithError(err).Warn("Failed to validate secret entries.")
		}
	}
	if o.validateOnly {
		logrus.Info("Validation succeeded and --validate-only is set, exiting")
		return
	}

	var client bitwarden.Client
	logrus.RegisterExitHandler(func() {
		if _, err := client.Logout(); err != nil {
			logrus.WithError(err).Error("failed to logout.")
		}
	})
	defer logrus.Exit(0)
	if o.dryRun {
		tmpFile, err := ioutil.TempFile("", "ci-secret-generator")
		if err != nil {
			logrus.WithError(err).Fatal("failed to create tempfile")
		}
		client, err = bitwarden.NewDryRunClient(tmpFile)
		if err != nil {
			logrus.WithError(err).Fatal("failed to create dryRun client")
		}
		logrus.Infof("Dry-Run enabled, writing secrets to %s", tmpFile.Name())
	} else {
		var err error
		client, err = bitwarden.NewClient(o.bwUser, o.bwPassword, func(s string) {
			secrets.Insert(s)
		})
		if err != nil {
			logrus.WithError(err).Fatal("failed to get Bitwarden client.")
		}
	}

	// Upload the output to bitwarden
	if err := updateSecrets(processedBwItems, client); err != nil {
		logrus.WithError(err).Fatal("Failed to update secrets.")
	}
	logrus.Info("Updated secrets.")
}

func bitwardenContextsFor(items []bitWardenItem) []secretbootstrap.BitWardenContext {
	var bitWardenContexts []secretbootstrap.BitWardenContext
	for _, bwItem := range items {
		for _, field := range bwItem.Fields {
			bitWardenContexts = append(bitWardenContexts, secretbootstrap.BitWardenContext{
				BWItem: bwItem.ItemName,
				Field:  field.Name,
			})
		}
		for _, attachment := range bwItem.Attachments {
			bitWardenContexts = append(bitWardenContexts, secretbootstrap.BitWardenContext{
				BWItem:     bwItem.ItemName,
				Attachment: attachment.Name,
			})
		}
		if bwItem.Password != "" {
			bitWardenContexts = append(bitWardenContexts, secretbootstrap.BitWardenContext{
				BWItem:    bwItem.ItemName,
				Attribute: secretbootstrap.AttributeTypePassword,
			})
		}
	}
	return bitWardenContexts
}

func validateContexts(contexts []secretbootstrap.BitWardenContext, config secretbootstrap.Config) error {
	var errs []error
	for _, needle := range contexts {
		var found bool
		for _, secret := range config.Secrets {
			for _, haystack := range secret.From {
				if reflect.DeepEqual(needle, haystack) {
					found = true
				}
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("could not find context %v in bootstrap config", needle))
		}
	}
	return utilerrors.NewAggregate(errs)
}
