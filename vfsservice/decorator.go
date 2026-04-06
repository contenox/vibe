package vfsservice

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/libtracker"
)

type activityTrackerDecorator struct {
	svc     Service
	tracker libtracker.ActivityTracker
}

// WithActivityTracker wraps a Service with activity logging.
func WithActivityTracker(svc Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{svc: svc, tracker: tracker}
}

var _ Service = (*activityTrackerDecorator)(nil)

func sanitizeFile(f *File) *File {
	if f == nil {
		return nil
	}
	return &File{
		ID:            f.ID,
		Path:          f.Path,
		Name:          f.Name,
		ParentID:      f.ParentID,
		Size:          f.Size,
		ContentType:   f.ContentType,
		IsDirectory:   f.IsDirectory,
	}
}

func (d *activityTrackerDecorator) CreateFile(ctx context.Context, file *File) (*File, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "create", "file",
		"path", file.Path, "contentType", file.ContentType, "size", fmt.Sprintf("%d", file.Size))
	defer end()
	result, err := d.svc.CreateFile(ctx, file)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, sanitizeFile(result))
	}
	return result, err
}

func (d *activityTrackerDecorator) GetFileByID(ctx context.Context, id string) (*File, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "file", "fileID", id)
	defer end()
	result, err := d.svc.GetFileByID(ctx, id)
	if err != nil {
		reportErr(err)
	}
	return result, err
}

func (d *activityTrackerDecorator) GetFolderByID(ctx context.Context, id string) (*Folder, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "folder", "folderID", id)
	defer end()
	result, err := d.svc.GetFolderByID(ctx, id)
	if err != nil {
		reportErr(err)
	}
	return result, err
}

func (d *activityTrackerDecorator) GetFilesByPath(ctx context.Context, path string) ([]File, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "file", "path", path)
	defer end()
	result, err := d.svc.GetFilesByPath(ctx, path)
	if err != nil {
		reportErr(err)
	}
	return result, err
}

func (d *activityTrackerDecorator) UpdateFile(ctx context.Context, file *File) (*File, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "update", "file",
		"fileID", file.ID, "contentType", file.ContentType, "size", fmt.Sprintf("%d", file.Size))
	defer end()
	result, err := d.svc.UpdateFile(ctx, file)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, sanitizeFile(result))
	}
	return result, err
}

func (d *activityTrackerDecorator) DeleteFile(ctx context.Context, id string) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "delete", "file", "fileID", id)
	defer end()
	err := d.svc.DeleteFile(ctx, id)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(id, nil)
	}
	return err
}

func (d *activityTrackerDecorator) CreateFolder(ctx context.Context, parentID, name string) (*Folder, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "create", "folder", "name", name)
	defer end()
	result, err := d.svc.CreateFolder(ctx, parentID, name)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, result)
	}
	return result, err
}

func (d *activityTrackerDecorator) RenameFile(ctx context.Context, fileID, newName string) (*File, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "rename", "file", "fileID", fileID, "newName", newName)
	defer end()
	result, err := d.svc.RenameFile(ctx, fileID, newName)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, sanitizeFile(result))
	}
	return result, err
}

func (d *activityTrackerDecorator) RenameFolder(ctx context.Context, folderID, newName string) (*Folder, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "rename", "folder", "folderID", folderID, "newName", newName)
	defer end()
	result, err := d.svc.RenameFolder(ctx, folderID, newName)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, result)
	}
	return result, err
}

func (d *activityTrackerDecorator) DeleteFolder(ctx context.Context, folderID string) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "delete", "folder", "folderID", folderID)
	defer end()
	err := d.svc.DeleteFolder(ctx, folderID)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(folderID, nil)
	}
	return err
}

func (d *activityTrackerDecorator) MoveFile(ctx context.Context, fileID, newParentID string) (*File, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "move", "file", "fileID", fileID, "newParentID", newParentID)
	defer end()
	result, err := d.svc.MoveFile(ctx, fileID, newParentID)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, sanitizeFile(result))
	}
	return result, err
}

func (d *activityTrackerDecorator) MoveFolder(ctx context.Context, folderID, newParentID string) (*Folder, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "move", "folder", "folderID", folderID, "newParentID", newParentID)
	defer end()
	result, err := d.svc.MoveFolder(ctx, folderID, newParentID)
	if err != nil {
		reportErr(err)
	} else {
		reportChange(result.ID, result)
	}
	return result, err
}
