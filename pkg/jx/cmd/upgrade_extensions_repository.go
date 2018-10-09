package cmd

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	jenkinsv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"

	"github.com/jenkins-x/jx/pkg/util"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"github.com/stoewer/go-strcase"

	"github.com/ghodss/yaml"
	"github.com/jenkins-x/jx/pkg/jx/cmd/templates"
	"github.com/spf13/cobra"
	"gopkg.in/AlecAivazis/survey.v1/terminal"
)

// CreateExtensionsRepositoryOptions the flags for running create cluster
type UpgradeExtensionsRepositoryOptions struct {
	UpgradeExtensionsOptions
	Flags      InitFlags
	InputFile  string
	OutputFile string
}

type UpgradeExtensionsRepositoryFlags struct {
}

type httpError struct {
	URL        string
	StatusCode int
	Status     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("Error fetching %s. %s", util.ColorError(e.URL), util.ColorError(e.Status))
}

var (
	upgradeExtensionsRepositoryLong = templates.LongDesc(`
		This command upgrades the jenkins-x-extensions-repository.lock.yaml file from a jenkins-x-extensions-repository.yaml file

`)

	upgradeExtensionsRepositoryExample = templates.Examples(`
		
        # Updates a file called jenkins-x-extensions-repository.lock.yaml from a file called  jenkins-x-extensions-repository.yaml in the same directory
        jx upgrade extensions repository
      
        # Allows the input and output file to specified
		jx upgrade extensions repository -i my-repo.yaml -o my-repo.lock.yaml

`)
)

// NewCmdGet creates a command object for the generic "init" action, which
// installs the dependencies required to run the jenkins-x platform on a Kubernetes cluster.
func NewCmdUpgradeExtensionsRepository(f Factory, in terminal.FileReader, out terminal.FileWriter, errOut io.Writer) *cobra.Command {
	options := &UpgradeExtensionsRepositoryOptions{
		UpgradeExtensionsOptions: UpgradeExtensionsOptions{
			CreateOptions: CreateOptions{
				CommonOptions: CommonOptions{
					Factory: f,
					In:      in,

					Out: out,
					Err: errOut,
				},
			},
		},
	}

	cmd := &cobra.Command{
		Use:     "repository",
		Short:   "Updates an extension repository",
		Long:    fmt.Sprintf(upgradeExtensionsRepositoryLong),
		Example: upgradeExtensionsRepositoryExample,
		Run: func(cmd2 *cobra.Command, args []string) {
			options.Cmd = cmd2
			options.Args = args
			err := options.Run()
			CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&options.InputFile, "input-file", "i", "jenkins-x-extensions-repository.yaml", "The input file to read to generate the .lock file")
	cmd.Flags().StringVarP(&options.OutputFile, "output-file", "o", "jenkins-x-extensions-repository.lock.yaml", "The output .lock file")
	return cmd
}

func (o *UpgradeExtensionsRepositoryOptions) Run() error {
	defRef := jenkinsv1.ExtensionDefinitionReferenceList{}
	err := defRef.LoadFromFile(o.InputFile)
	if err != nil {
		return err
	}
	oldLock := jenkinsv1.ExtensionRepositoryLockList{}
	err = oldLock.LoadFromFile(o.OutputFile)
	if err != nil {
		return err
	}
	oldVersion := oldLock.Version
	oldLockNameMap := make(map[string]jenkinsv1.ExtensionSpec, 0)
	for _, l := range oldLock.Extensions {
		oldLockNameMap[l.FullyQualifiedName()] = l
	}
	newLock := jenkinsv1.ExtensionRepositoryLockList{
		Extensions: make([]jenkinsv1.ExtensionSpec, 0),
	}
	newLock.Version = oldVersion + 1

	lookupByName := make(map[string]jenkinsv1.ExtensionSpec, 0)
	lookupByUUID := make(map[string]jenkinsv1.ExtensionSpec, 0)
	for _, c := range defRef.Remotes {
		e, err := o.walkRemote(c.Remote, c.Tag, oldLockNameMap)
		if err != nil {
			return err
		}
		newLock.Extensions = append(newLock.Extensions, e...)
		for _, r := range e {
			lookupByName[r.FullyQualifiedName()] = r
			lookupByUUID[r.UUID] = r
		}
	}
	uuidResolveErrors := make([]string, 0)
	// Second pass over extensions to allow us to do things like resolve fqns into UUIDs
	for i, lock := range newLock.Extensions {
		newLock.Extensions[i].Children, err = o.FixChildren(lock, lookupByName, lookupByUUID, &uuidResolveErrors)
		if err != nil {
			return err
		}
	}
	if len(uuidResolveErrors) > 0 {
		bytes, err := yaml.Marshal(newLock)
		if err != nil {
			return err
		}
		errFile, err := ioutil.TempFile("", o.OutputFile)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(errFile.Name(), bytes, 0755)
		if err != nil {
			return err
		}
		return errors.New(fmt.Sprintf("Cannot resolve children %s in repository. Partial .lock file written to %s.", uuidResolveErrors, errFile.Name()))
	}
	bytes, err := yaml.Marshal(newLock)
	if err != nil {
		return err
	}
	log.Printf("Updating extensions repository from %s to %s. Changes are %s\n", util.ColorInfo(oldLock.Version), util.ColorInfo(newLock.Version), util.ColorInfo("Unknown"))
	err = ioutil.WriteFile(o.OutputFile, bytes, 0755)
	if err != nil {
		return err
	}
	return nil
}

func (o *UpgradeExtensionsRepositoryOptions) walkRemote(remote string, tag string, oldLockNameMap map[string]jenkinsv1.ExtensionSpec) (extensions []jenkinsv1.ExtensionSpec, err error) {
	result := make([]jenkinsv1.ExtensionSpec, 0)
	if strings.HasPrefix(remote, "github.com") {
		s := strings.Split(remote, "/")
		if len(s) != 3 {
			errors.New(fmt.Sprintf("Cannot parse extension path %s", util.ColorInfo(remote)))
		}
		org := s[1]
		repo := s[2]
		if tag == "latest" {
			tag, err = util.GetLatestVersionStringFromGitHub(org, repo)
			if err != nil {
				return result, err
			}
			tag = fmt.Sprintf("v%s", tag)
		}
		definitionsUrl := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", org, repo, tag)
		definitionsFileUrl := fmt.Sprintf("%s/jenkins-x-extension-definitions.yaml", definitionsUrl)
		extensionDefinitions := jenkinsv1.ExtensionDefinitionList{}
		extensionDefinitions.LoadFromURL(definitionsFileUrl, remote, tag)
		for _, ed := range extensionDefinitions.Extensions {
			// It's best practice to assign a UUID to an extension, but if it doesn't have one we
			// try to give it the one it had last
			UUID := ed.UUID
			if UUID == "" {
				UUID = oldLockNameMap[ed.FullyQualifiedName()].UUID
			}
			// If the UUID is still empty, generate one
			if UUID == "" {
				UUID = uuid.New()
				log.Printf("No UUID found for %s. Generated UUID %s, please update your extension definition "+
					"accordingly.", ed.FullyQualifiedName(), UUID)
			}
			var script string
			children := make([]string, 0)
			// If the children is present, there is no script
			if len(ed.Children) == 0 {
				scriptFile := ed.ScriptFile
				if scriptFile == "" {
					scriptFile = fmt.Sprintf("%s.sh", strings.ToLower(strcase.SnakeCase(ed.Name)))
				}
				script, err = o.LoadAsStringFromURL(fmt.Sprintf("%s/%s", definitionsUrl, scriptFile))
				if err != nil {
					return result, err
				}
			} else {
				for _, c := range ed.Children {
					if c.UUID != "" {
						children = append(children, c.UUID)
					} else {
						children = append(children, c.FullyQualifiedName())
					}
					if c.Remote != "" {
						t := c.Tag
						if t == "" {
							t = "latest"
						}
						r, err := o.walkRemote(c.Remote, c.Tag, oldLockNameMap)
						if err != nil {
							return result, err
						}
						result = append(result, r...)
					}
				}
			}
			eLock := jenkinsv1.ExtensionSpec{
				Name:        ed.Name,
				Namespace:   ed.Namespace,
				Version:     strings.TrimPrefix(tag, "v"),
				UUID:        UUID,
				Description: ed.Description,
				Parameters:  ed.Parameters,
				When:        ed.When,
				Given:       ed.Given,
				Script:      script,
				Children:    children,
			}
			result = append(result, eLock)
		}
		return result, nil
	} else {
		return result, errors.New(fmt.Sprintf("Only github.com is supported, use a format like %s", util.ColorInfo("github.com/jenkins-x/ext-jacoco")))
	}
}

func (o *UpgradeExtensionsRepositoryOptions) FixChildren(extension jenkinsv1.ExtensionSpec, lookupByName map[string]jenkinsv1.ExtensionSpec, lookupByUUID map[string]jenkinsv1.ExtensionSpec, resolveErrors *[]string) (children []string, err error) {
	children = make([]string, 0)
	for _, childUUID := range extension.Children {
		if uuid.Parse(childUUID) == nil {
			if c, ok := lookupByName[childUUID]; ok {
				log.Printf("We recommend you explicitly specify the UUID for childUUID %s on extension %s as this will stop the "+
					"extension breaking if names are changed.\n"+
					"If you are the maintainer of the extension definition add \n"+
					"\n"+
					"      UUID: %s\n"+
					"\n"+
					"to the childUUID definition. \n"+
					"If you aren't the maintainer of the extension definition we recommend you contact them and ask"+
					"them to add the UUID.\n"+
					"\n"+
					"This does not stop you using the extension, as we have been able to discover and attach the "+
					"UUID to the childUUID based on the fully qualified name.\n", util.ColorWarning(childUUID), util.ColorWarning(extension.FullyQualifiedName()), util.ColorWarning(c.UUID))
				childUUID = c.UUID
			} else {
				// Record the error in the loop, but don't error until end. This allows us to report all the errors
				// up front
				*resolveErrors = append(*resolveErrors, childUUID)
			}
		}
		if _, ok := lookupByUUID[childUUID]; ok {
			children = append(children, childUUID)
		} else {
			return children, errors.New(fmt.Sprintf("Unable to find UUID for %s", util.ColorError(extension.FullyQualifiedName())))
		}
	}
	return children, nil
}

func (o *UpgradeExtensionsRepositoryOptions) LoadAsStringFromURL(url string) (result string, err error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("%s?version=%d", url, time.Now().UnixNano()/int64(time.Millisecond)))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &httpError{
			URL:        url,
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
		}
	}

	defer resp.Body.Close()
	if err != nil {
		return "", err
	}

	bytes, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return "", err
	}
	return string(bytes), nil
}
