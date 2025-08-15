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

// PruneAllMavenMetadata rebuilds all maven-metadata.xml files to remove references to non-existent artifacts.
func PruneAllMavenMetadata(ctx context.Context) error {
	debugSession := setting.Packages.DebugMavenMetadataPrune
	log.Debug("Starting PruneAllMavenMetadata with debugSession: %t", debugSession)

	versions, err := packages.GetVersionsByPackageType(ctx, 0, packages.TypeMaven)
	if err != nil {
		return fmt.Errorf("PruneAllMavenMetadata: failed to retrieve Maven package versions: %w", err)
	}

	var errors []error
	var results []string
	totalPruned := 0

	for _, version := range versions {
		if !isSnapshotVersion(version.Version) {
			continue
		}

		pruned, err := pruneMetadata(ctx, version.ID, debugSession)
		if err != nil {
			errors = append(errors, fmt.Errorf("PruneAllMavenMetadata: version '%s' (ID: %d): %w", version.Version, version.ID, err))
		}
		if pruned {
			totalPruned++
			results = append(results, fmt.Sprintf("version %d", version.ID))
		}
	}

	if len(errors) > 0 {
		for _, err := range errors {
			log.Warn("PruneAllMavenMetadata: Error during pruning: %v", err)
		}
		return fmt.Errorf("PruneAllMavenMetadata: pruning completed with %d errors: %v", len(errors), errors)
	}

	if totalPruned > 0 {
		log.Info("PruneAllMavenMetadata: successfully pruned %d metadata files: %s", totalPruned, strings.Join(results, ", "))
	} else {
		log.Debug("Completed PruneAllMavenMetadata: no metadata files needed pruning")
	}
	return nil
}

func pruneMetadata(ctx context.Context, versionID int64, debugSession bool) (bool, error) {
	log.Debug("Starting pruneMetadata for versionID: %d with debugSession: %t", versionID, debugSession)

	metadataFile, err := packages.GetFileForVersionByName(ctx, versionID, "maven-metadata.xml", packages.EmptyFileKey)
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: failed to retrieve Maven metadata file for version ID %d: %w", versionID, err)
	}

	pb, err := packages.GetBlobByID(ctx, metadataFile.BlobID)
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: failed to get package blob: %w", err)
	}

	rc, _, _, err := packages_service.OpenBlobForDownload(ctx, metadataFile, pb, "", nil, true)
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: failed to get package file stream: %w", err)
	}
	defer rc.Close()

	metadata, err := maven.ParseSnapshotVersionMetadataXML(rc)
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: failed to parse metadata xml: %w", err)
	}

	allFiles, err := packages.GetFilesByVersionID(ctx, versionID)
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: failed to get files for version: %w", err)
	}

	existingFiles := make(map[string]bool)
	for _, file := range allFiles {
		existingFiles[file.Name] = true
	}

	filtered := metadata.Versioning.SnapshotVersions[:0]
	maxBuild := 0
	for _, sv := range metadata.Versioning.SnapshotVersions {
		fileName := fmt.Sprintf("%s-%s", metadata.ArtifactID, sv.Value)
		if sv.Classifier != "" {
			fileName = fmt.Sprintf("%s-%s", fileName, sv.Classifier)
		}
		fileName = fmt.Sprintf("%s.%s", fileName, sv.Extension)

		if existingFiles[fileName] {
			build, err := buildNumberFromValue(sv.Value)
			if err != nil {
				return false, err
			}
			filtered = append(filtered, sv)
			if build > maxBuild {
				maxBuild = build
			}
		}
	}
	metadata.Versioning.SnapshotVersions = filtered
	metadata.Versioning.Snapshot.BuildNumber = strconv.Itoa(maxBuild)

	if len(metadata.Versioning.SnapshotVersions) == len(filtered) {
		return false, nil
	}

	metadata.Versioning.SnapshotVersions = filtered
	metadata.Versioning.Snapshot.BuildNumber = strconv.Itoa(maxBuild)

	if debugSession {
		log.Info("pruneMetadata: Debug session active. Would have rebuilt metadata for versionID %d", versionID)
		return true, nil
	}

	buf := bytes.Buffer{}
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(metadata); err != nil {
		return false, fmt.Errorf("pruneMetadata: encode xml: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return false, fmt.Errorf("pruneMetadata: flush xml: %w", err)
	}

	hashedBuf, err := packages_module.CreateHashedBufferFromReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: create buffer: %w", err)
	}

	pv, err := packages.GetVersionByID(ctx, metadataFile.VersionID)
	if err != nil {
		return false, fmt.Errorf("pruneMetadata: get version: %w", err)
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
	if err != nil {
		return false, err
	}
	return true, nil
}

// PruneMetadataForDeletedFile is called when a file is deleted to check if the metadata needs to be updated.
func PruneMetadataForDeletedFile(ctx context.Context, file *packages.PackageFile) error {
	// Get the version information
	pv, err := packages.GetVersionByID(ctx, file.VersionID)
	if err != nil {
		return fmt.Errorf("PruneMetadataForDeletedFile: failed to get version: %w", err)
	}

	if !isSnapshotVersion(pv.Version) {
		return nil
	}

	if strings.HasSuffix(file.Name, ".pom") || strings.HasSuffix(file.Name, ".jar") || strings.HasSuffix(file.Name, ".war") || strings.HasSuffix(file.Name, ".ear") {
		_, err := pruneMetadata(ctx, file.VersionID, setting.Packages.DebugMavenMetadataPrune)
		return err
	}
	return nil
}
