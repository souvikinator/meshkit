package helm

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/layer5io/meshkit/encoding"
	"github.com/layer5io/meshkit/utils"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/downloader"
)

func extractSemVer(versionConstraint string) string {
	reg := regexp.MustCompile(`v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+)?$`)
	match := reg.Find([]byte(versionConstraint))
	if match != nil {
		return string(match)
	}
	return ""
}

// DryRun a given helm chart to convert into k8s manifest
func DryRunHelmChart(chart *chart.Chart, kubernetesVersion string) ([]byte, error) {
	actconfig := new(action.Configuration)
	act := action.NewInstall(actconfig)
	act.ReleaseName = chart.Metadata.Name
	act.Namespace = "default"
	act.DryRun = true
	act.IncludeCRDs = true
	act.ClientOnly = true

	kubeVersion := kubernetesVersion
	if chart.Metadata.KubeVersion != "" {
		extractedVersion := extractSemVer(chart.Metadata.KubeVersion)

		if extractedVersion != "" {
			kubeVersion = extractedVersion
		}
	}

	if kubeVersion != "" {
		act.KubeVersion = &chartutil.KubeVersion{
			Version: kubeVersion,
		}
	}

	rel, err := act.Run(chart, nil)
	if err != nil {
		return nil, ErrDryRunHelmChart(err, chart.Name())
	}
	var manifests bytes.Buffer
	_, err = manifests.Write([]byte(strings.TrimSpace(rel.Manifest)))
	if err != nil {
		return nil, ErrDryRunHelmChart(err, chart.Name())
	}
	return manifests.Bytes(), nil
}

// Takes in the directory and converts HelmCharts/multiple manifests into a single K8s manifest
func ConvertToK8sManifest(path, kubeVersion string, w io.Writer) error {
	info, err := os.Stat(path)
	if err != nil {
		return utils.ErrReadDir(err, path)
	}
	helmChartPath := path
	if !info.IsDir() {
		helmChartPath, _ = strings.CutSuffix(path, filepath.Base(path))
	}
	if IsHelmChart(helmChartPath) {
		err := LoadHelmChart(helmChartPath, w, kubeVersion)
		if err != nil {
			return err
		}
		// If not a helm chart then assume k8s manifest.
		// Add introspection for compose files later on.
	} else if utils.IsYaml(path) {
		pathInfo, _ := os.Stat(path)
		if pathInfo.IsDir() {
			err := filepath.WalkDir(path, func(path string, d fs.DirEntry, _err error) error {
				err := writeToFile(w, path)
				if err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			err := writeToFile(w, path)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// writes in form of yaml files
func writeToFile(w io.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return utils.ErrReadFile(err, path)
	}

	byt, err := encoding.ToYaml(data)
	if err != nil {
		return utils.ErrWriteFile(err, path)
	}

	_, err = w.Write(byt)
	if err != nil {
		return utils.ErrWriteFile(err, path)
	}
	_, err = w.Write([]byte("\n---\n"))
	if err != nil {
		return utils.ErrWriteFile(err, path)
	}

	return nil
}

// Exisitence of Chart.yaml/Chart.yml indicates the directory contains a helm chart
func IsHelmChart(dirPath string) bool {
	_, err := os.Stat(filepath.Join(dirPath, "Chart.yaml"))
	if err != nil {
		_, err = os.Stat(filepath.Join(dirPath, "Chart.yml"))
		if err != nil {
			return false
		}
	}
	return true
}

func LoadHelmChart(path string, w io.Writer, kubeVersion string) error {
	// Create a client for managing chart dependencies
	dm := downloader.Manager{
		Out:       w,
		ChartPath: path,
	}

	// First load the chart without resolving dependencies
	chart, err := loader.Load(path)
	if err != nil {
		return ErrLoadHelmChart(err, path)
	}

	// Check if the chart has dependencies and resolve them
	if len(chart.Metadata.Dependencies) > 0 {
		// Update/download all dependencies - this will fetch and process all dependencies
		// including partials and any other charts specified in Chart.yaml
		err = dm.Update()
		if err != nil {
			// TODO: fail forward
			return ErrLoadHelmChart(fmt.Errorf("Failed to download Helm chart dependencies: %v", err), path)
		}

		// Reload the chart after dependencies are resolved to include the newly downloaded
		// dependencies and their templates
		chart, err = loader.Load(path)
		if err != nil {
			return ErrLoadHelmChart(err, path)
		}
	}

	// Perform a dry run to get all rendered templates with dependencies resolved
	manifests, err := DryRunHelmChart(chart, kubeVersion)
	if err != nil {
		return ErrLoadHelmChart(err, path)
	}

	// clean up the manifests for any nil values
	// while rendering if the value.yml is empty the placeholders gets replaced with %s<nil>
	// we remove them for now
	manifests = cleanNilValues(manifests)

	if _, err := w.Write(manifests); err != nil {
		return fmt.Errorf("Failed to write manifests to writer: %v", err)
	}

	return nil
}

func cleanNilValues(data []byte) []byte {
	// First clean simple nil values with surrounding content
	cleaned := bytes.ReplaceAll(data, []byte(" %!s(<nil>)"), []byte(""))

	// Replace list items containing only nil with empty object
	nilItemRegex := regexp.MustCompile(`(?m)(^\s*-\s+)%!s\(<nil>\)\s*$`)
	cleaned = nilItemRegex.ReplaceAll(cleaned, []byte("${1}{}"))

	// Clean up any empty lines that might have been left
	emptyLineRegex := regexp.MustCompile(`(?m)^\s*\n\s*$`)
	cleaned = emptyLineRegex.ReplaceAll(cleaned, []byte("\n"))

	return cleaned
}

func writeToWriter(w io.Writer, data []byte) error {
	trimmedData := bytes.TrimSpace(data)

	if len(trimmedData) == 0 {
		return nil
	}

	// Check if the document already starts with separators
	startsWithSeparator := bytes.HasPrefix(trimmedData, []byte("---"))

	// If it doesn't start with ---, add one
	if !startsWithSeparator {
		if _, err := w.Write([]byte("---\n")); err != nil {
			return err
		}
	}

	if _, err := w.Write(trimmedData); err != nil {
		return err
	}

	_, err := w.Write([]byte("\n"))
	return err
}
