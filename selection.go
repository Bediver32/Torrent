package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

type fileEntry struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Done  int64  `json:"done"`
	Skip  bool   `json:"skip"`
}

type fileList struct {
	Name  string      `json:"name"`
	Files []fileEntry `json:"files"`
}

func readMetainfo(data []byte) (*metainfo.MetaInfo, error) {
	if len(data) == 0 {
		return nil, errors.New("выберите файл .torrent")
	}
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать .torrent: %w", err)
	}
	return mi, nil
}

func inspectMetainfo(data []byte) (fileList, error) {
	mi, err := readMetainfo(data)
	if err != nil {
		return fileList{}, err
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return fileList{}, fmt.Errorf("не удалось прочитать список файлов: %w", err)
	}
	if info.PieceLength <= 0 {
		return fileList{}, errors.New("некорректный размер блока в .torrent")
	}
	list := fileList{Name: info.BestName(), Files: []fileEntry{}}
	for i, file := range info.UpvertedFiles() {
		if !strings.ContainsRune(file.Attr, 'p') {
			list.Files = append(list.Files, fileEntry{Index: i, Path: file.DisplayPath(&info), Size: file.Length})
		}
	}
	return list, nil
}

func validateSkipped(skipped []int, count int) error {
	slices.Sort(skipped)
	for i, index := range skipped {
		if index < 0 || index >= count || (i > 0 && skipped[i-1] == index) {
			return errors.New("список файлов изменился или содержит неверные номера")
		}
	}
	return nil
}

func (t *task) filesReady() bool {
	if t.torrent == nil {
		return false
	}
	select {
	case <-t.torrent.GotInfo():
		return true
	default:
		return false
	}
}

func (t *task) applySelection() {
	for i, f := range t.torrent.Files() {
		_, skip := slices.BinarySearch(t.Skipped, i)
		priority := torrent.PiecePriorityNormal
		if skip || f.Length() == 0 || strings.ContainsRune(f.FileInfo().Attr, 'p') {
			priority = torrent.PiecePriorityNone
		}
		f.SetPriority(priority)
	}
}

func (q *queue) findFiles(id string) (*task, error) {
	for _, t := range q.tasks {
		if t.ID != id {
			continue
		}
		if !t.filesReady() {
			return nil, errors.New("дождитесь получения метаданных")
		}
		return t, nil
	}
	return nil, errors.New("торрент уже удалён")
}

func (q *queue) files(id string) (fileList, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.findFiles(id)
	if err != nil {
		return fileList{}, err
	}
	list := fileList{Name: t.torrent.Name(), Files: []fileEntry{}}
	for i, f := range t.torrent.Files() {
		if strings.ContainsRune(f.FileInfo().Attr, 'p') {
			continue
		}
		_, skip := slices.BinarySearch(t.Skipped, i)
		entry := fileEntry{Index: i, Path: f.DisplayPath(), Size: f.Length(), Skip: skip}
		if t.opened {
			entry.Done = verifiedBytes(f)
		}
		list.Files = append(list.Files, entry)
	}
	return list, nil
}

func (q *queue) selectFiles(id string, skipped []int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.findFiles(id)
	if err != nil {
		return err
	}
	if err := validateSkipped(skipped, len(t.torrent.Files())); err != nil {
		return err
	}
	old := t.Skipped
	if slices.Equal(old, skipped) {
		return nil
	}
	t.Skipped = skipped
	if err := q.save(); err != nil {
		t.Skipped = old
		return err
	}
	if t.opened {
		// A web seed may have an open HTTP request that spans several files.
		// Cancel outstanding requests when a new file is excluded.
		restart := !t.Paused && t.failure == "" && slices.ContainsFunc(skipped, func(i int) bool {
			_, alreadySkipped := slices.BinarySearch(old, i)
			return !alreadySkipped
		})
		if restart {
			t.torrent.DisallowDataDownload()
		}
		t.applySelection()
		if restart {
			t.torrent.AllowDataDownload()
		}
	}
	q.refresh(t, time.Now())
	return nil
}

func verifiedBytes(f *torrent.File) int64 {
	var done int64
	for _, piece := range f.State() {
		if piece.Complete {
			done += piece.Bytes
		}
	}
	return done
}

func (t *task) progress(r *row) (selected bool) {
	bt := t.torrent
	if len(t.Skipped) == 0 && !t.hasPadding {
		r.Size = bt.Length()
		r.Done = bt.BytesCompleted()
		r.Complete = bt.Complete().Bool()
		return true
	}
	for i, f := range bt.Files() {
		_, skip := slices.BinarySearch(t.Skipped, i)
		if skip || strings.ContainsRune(f.FileInfo().Attr, 'p') {
			continue
		}
		r.Size += f.Length()
		r.Done += verifiedBytes(f)
		selected = true
	}
	r.Complete = selected && r.Done == r.Size
	return selected
}
