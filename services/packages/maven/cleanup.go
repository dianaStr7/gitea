package maven

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"code.gitea.io/gitea/models/packages"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/log"
	packages_module "code.gitea.io/gitea/modules/packages"
	"code.gitea.io/gitea/modules/packages/maven"
	"code.gitea.io/gitea/modules/setting"
	packages_service "code.gitea.io/gitea/services/packages"
)

// CleanupSnapshotVersions removes outdated files for SNAPHOT versions for all Maven packages.
func CleanupSnapshotVersions(ctx context.Context) error {
	retainBuilds := setting.Packages.RetainMavenSnapshotBuilds
	debugSession := setting.Packages.DebugMavenCleanup
	log.Debug("Starting Maven CleanupSnapshotVersions with retainBuilds: %d, debugSession: %t", retainBuilds, debugSession)

	if retainBuilds < 1 {
		log.Info("Maven CleanupSnapshotVersions skipped because retainBuilds is set to less than 1")
		return nil
	}

	versions, err := packages.GetVersionsByPackageType(ctx, 0, packages.TypeMaven)
	if err != nil {
		return fmt.Errorf("maven CleanupSnapshotVersions: failed to retrieve Maven package versions: %w", err)
	}

	var errors []error
	var results []string
	totalCleaned := 0

	for _, version := range versions {
		if !isSnapshotVersion(version.Version) {
			continue
		}

		cleaned, err := cleanSnapshotFiles(ctx, version.ID, retainBuilds, debugSession)
		if err != nil {
			errors = append(errors, fmt.Errorf("maven CleanupSnapshotVersions: version '%s' (ID: %d): %w", version.Version, version.ID, err))
		}
		if cleaned > 0 {
			totalCleaned += cleaned
			results = append(results, fmt.Sprintf("%d from version %d", cleaned, version.ID))
		}
	}

	if len(errors) > 0 {
		for _, err := range errors {
			log.Warn("maven CleanupSnapshotVersions: Error during cleanup: %v", err)
		}
		return fmt.Errorf("maven CleanupSnapshotVersions: cleanup completed with %d errors: %v", len(errors), errors)
	}

	if totalCleaned > 0 {
		log.Info("maven CleanupSnapshotVersions: successfully cleaned %d files: %s", totalCleaned, strings.Join(results, ", "))
	} else {
		log.Debug("Completed Maven CleanupSnapshotVersions: no files needed cleaning")
	}
	return nil
}

func isSnapshotVersion(version string) bool {
	return strings.HasSuffix(version, "-SNAPSHOT")
}

func cleanSnapshotFiles(ctx context.Context, versionID int64, retainBuilds int, debugSession bool) (int, error) {
	log.Debug("Starting Maven cleanSnapshotFiles for versionID: %d with retainBuilds: %d, debugSession: %t", versionID, retainBuilds, debugSession)

	metadataFile, metadata, err := getSnapshotMetadata(ctx, versionID)
	if err != nil {
		return 0, fmt.Errorf("cleanSnapshotFiles: failed to retrieve Maven metadata for version ID %d: %w", versionID, err)
	}

	buildNumber, _ := strconv.Atoi(metadata.Versioning.Snapshot.BuildNumber)
	thresholdBuildNumber := buildNumber - retainBuilds
	if thresholdBuildNumber <= 0 {
		log.Debug("cleanSnapshotFiles: No files to clean up, threshold <= 0 for versionID %d", versionID)
		return 0, nil
	}

	// Collect possible file endings: classifier and extension
	var fileEndings []string
	valuesToPrune := make(map[string]struct{})

	for _, sv := range metadata.Versioning.SnapshotVersions {
		ending := ""
		if sv.Classifier != "" {
			ending += "-" + sv.Classifier
		}
		if sv.Extension != "" {
			ending += "." + sv.Extension
		}
		if ending != "" {
			fileEndings = append(fileEndings, ending)
		}
	}

	filesToRemove, skippedFiles, err := packages.GetFilesBelowBuildNumber(ctx, versionID, thresholdBuildNumber, fileEndings...)
	if err != nil {
		return 0, fmt.Errorf("cleanSnapshotFiles: failed to retrieve files for version ID %d: %w", versionID, err)
	}

	if debugSession {
		var fileNamesToRemove, skippedFileNames []string

		for _, file := range filesToRemove {
			fileNamesToRemove = append(fileNamesToRemove, file.Name)
		}

		for _, file := range skippedFiles {
			skippedFileNames = append(skippedFileNames, file.Name)
		}

		log.Info("cleanSnapshotFiles: Debug session active. Files to remove: %v, Skipped files: %v", fileNamesToRemove, skippedFileNames)
		return len(filesToRemove), nil
	}

	for _, file := range filesToRemove {
		log.Debug("Removing file '%s' below threshold %d", file.Name, thresholdBuildNumber)
		if err := packages_service.DeletePackageFile(ctx, file); err != nil {
			return 0, fmt.Errorf("maven cleanSnapshotFiles: failed to delete file '%s': %w", file.Name, err)
		}

		// Optionally prune metadata after each file deletion
		if err := PruneMetadataForDeletedFile(ctx, file); err != nil {
			log.Warn("maven cleanSnapshotFiles: failed to prune metadata after deleting file '%s': %v", file.Name, err)
		}
	}

	if len(filesToRemove) > 0 {
		if err := pruneSnapshotMetadataWithExistingData(ctx, metadataFile, metadata, thresholdBuildNumber); err != nil {
			return 0, fmt.Errorf("maven cleanSnapshotFiles: failed to prune metadata for version ID %d: %w", versionID, err)
		}
	}

	log.Debug("Completed Maven cleanSnapshotFiles for versionID: %d", versionID)
	return len(filesToRemove), nil
}

// getSnapshotMetadata retrieves and parses the maven-metadata.xml file for a version
func getSnapshotMetadata(ctx context.Context, versionID int64) (*packages.PackageFile, *maven.SnapshotMetadataXML, error) {
	metadataFile, err := packages.GetFileForVersionByName(ctx, versionID, "maven-metadata.xml", packages.EmptyFileKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to retrieve Maven metadata file: %w", err)
	}

	pb, err := packages.GetBlobByID(ctx, metadataFile.BlobID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get package blob: %w", err)
	}

	content, _, _, err := packages_service.OpenBlobForDownload(ctx, metadataFile, pb, "", nil, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get package file stream: %w", err)
	}
	defer content.Close()

	metadata, err := maven.ParseSnapshotVersionMetadataXML(content)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse maven-metadata.xml: %w", err)
	}

	return metadataFile, metadata, nil
}

func pruneSnapshotMetadataWithExistingData(ctx context.Context, metadataFile *packages.PackageFile, metadata *maven.SnapshotMetadataXML, threshold int) error {
	filtered := metadata.Versioning.SnapshotVersions[:0]
	maxBuild := 0
	for _, sv := range metadata.Versioning.SnapshotVersions {
		build, err := buildNumberFromValue(sv.Value)
		if err != nil {
			filtered = append(filtered, sv)
			continue
		}
		if build > threshold {
			filtered = append(filtered, sv)
			if build > maxBuild {
				maxBuild = build
			}
		}
	}
	metadata.Versioning.SnapshotVersions = filtered
	metadata.Versioning.Snapshot.BuildNumber = strconv.Itoa(maxBuild)

	buf := bytes.Buffer{}
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(metadata); err != nil {
		return fmt.Errorf("pruneSnapshotMetadata: encode xml: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return fmt.Errorf("pruneSnapshotMetadata: flush xml: %w", err)
	}

	hashedBuf, err := packages_module.CreateHashedBufferFromReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("pruneSnapshotMetadata: create buffer: %w", err)
	}

	pv, err := packages.GetVersionByID(ctx, metadataFile.VersionID)
	if err != nil {
		return fmt.Errorf("pruneSnapshotMetadata: get version: %w", err)
	}

	_, err = packages_service.AddFileToPackageVersionInternal(ctx, pv, &packages_service.PackageFileCreationInfo{
		PackageFileInfo: packages_service.PackageFileInfo{
			Filename:     metadataFile.Name,
			CompositeKey: metadataFile.CompositeKey,
		},
		Creator:           user_model.NewGhostUser(),
		Data:              hashedBuf,
		IsLead:            metadataFile.IsLead,
		OverwriteExisting: true,
	})
	return err
}

func buildNumberFromValue(value string) (int, error) {
	idx := strings.LastIndex(value, "-")
	if idx == -1 {
		return 0, fmt.Errorf("buildNumberFromValue: invalid snapshot value '%s'", value)
	}
	num, err := strconv.Atoi(value[idx+1:])
	if err != nil {
		return 0, fmt.Errorf("buildNumberFromValue: %w", err)
	}
	return num, nil
}
