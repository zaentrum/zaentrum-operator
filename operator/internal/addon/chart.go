package addon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// maxUnpackedSize bounds a chart archive's content once decompressed. The
// archive itself is at most MaxArchiveSize, but gzip expands far beyond that,
// and the operator holds the whole chart in memory to render it.
const maxUnpackedSize = 20 << 20

// LoadChart loads a chart archive (.tgz).
func LoadChart(archive []byte) (*chart.Chart, error) {
	if len(archive) > MaxArchiveSize {
		return nil, errArchiveTooLarge
	}
	if err := checkUnpackedSize(archive, maxUnpackedSize); err != nil {
		return nil, err
	}
	chrt, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("load chart: %w", err)
	}
	return chrt, nil
}

// checkUnpackedSize walks the archive once, counting content bytes, before the
// Helm loader (which has no such limit) reads it into memory.
func checkUnpackedSize(archive []byte, limit int64) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("load chart: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		if _, err := tr.Next(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("load chart: %w", err)
		}
		n, err := io.Copy(io.Discard, io.LimitReader(tr, limit-total+1))
		if err != nil {
			return fmt.Errorf("load chart: %w", err)
		}
		if total += n; total > limit {
			return fmt.Errorf("chart archive unpacks to more than %d MiB", limit>>20)
		}
	}
}

// CheckConventions returns the ways a chart breaks the addon chart conventions.
func CheckConventions(chrt *chart.Chart) []string {
	var out []string
	md := chrt.Metadata
	if md.APIVersion != chart.APIVersionV2 {
		out = append(out, fmt.Sprintf("Chart.yaml: apiVersion %s not supported (v2 required)", md.APIVersion))
	}
	if md.Type != "" && md.Type != "application" {
		out = append(out, fmt.Sprintf("Chart.yaml: type %s cannot be installed (application required)", md.Type))
	}
	if md.Annotations[ChartAnnotationAddon] != "true" {
		out = append(out, `Chart.yaml: annotation zaentrum.io/addon: "true" is required`)
	}
	if md.Annotations[ChartAnnotationPrimary] == "" {
		out = append(out, "Chart.yaml: annotation zaentrum.io/primary is required")
	}
	for _, crd := range chrt.CRDObjects() {
		out = append(out, crd.Filename+": CustomResourceDefinition not allowed")
	}
	return out
}

// ChartInfo describes a chart for the plan.
func ChartInfo(chrt *chart.Chart, digest string) zaentrumv1alpha1.AddonChartInfo {
	md := chrt.Metadata
	info := zaentrumv1alpha1.AddonChartInfo{
		Name:        md.Name,
		Version:     md.Version,
		AppVersion:  md.AppVersion,
		Description: md.Description,
		Digest:      digest,
	}
	if len(md.Annotations) > 0 {
		info.Annotations = make(map[string]string, len(md.Annotations))
		for k, v := range md.Annotations {
			info.Annotations[k] = v
		}
	}
	return info
}
