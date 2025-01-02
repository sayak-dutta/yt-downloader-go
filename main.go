package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kkdai/youtube/v2"
)

type Config struct {
	OutputDir     string
	MaxConcurrent int
	Quality       string
	MetadataOnly  bool
	MP3Only       bool
}

type VideoInfo struct {
	Title       string
	Author      string
	Duration    time.Duration
	Description string
}

type Downloader struct {
	client *youtube.Client
	config Config
	guard  chan struct{}
	logger *log.Logger
}

func NewDownloader(config Config) *Downloader {
	return &Downloader{
		client: &youtube.Client{},
		config: config,
		guard:  make(chan struct{}, config.MaxConcurrent),
		logger: log.New(os.Stdout, "[YouTube Downloader] ", log.LstdFlags),
	}
}

func (d *Downloader) downloadVideo(ctx context.Context, video *youtube.Video, wg *sync.WaitGroup) error {
	defer wg.Done()

	d.guard <- struct{}{}
	defer func() { <-d.guard }()

	info := VideoInfo{
		Title:       video.Title,
		Author:      video.Author,
		Duration:    video.Duration,
		Description: video.Description,
	}

	safeTitle := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) {
			return '-'
		}
		return r
	}, info.Title)

	extension := ".mp4"
	if d.config.MP3Only {
		extension = ".mp3"
	}

	tempPath := filepath.Join(d.config.OutputDir, safeTitle+"_temp.mp4")
	finalPath := filepath.Join(d.config.OutputDir, safeTitle+extension)

	// For MP4: Get both video and audio formats
	var videoFormat, audioFormat *youtube.Format

	if !d.config.MP3Only {
		// Get best video format
		formats := video.Formats
		var videoFormats youtube.FormatList
		for _, format := range formats {
			if format.Quality == "hd720" && format.AudioChannels == 0 {
				videoFormats = append(videoFormats, format)
			}
		}
		if len(videoFormats) == 0 {
			for _, format := range formats {
				if format.Quality == "medium" && format.AudioChannels == 0 {
					videoFormats = append(videoFormats, format)
				}
			}
		}
		if len(videoFormats) > 0 {
			videoFormat = &videoFormats[0]
		}

		// Get best audio format
		var audioFormats youtube.FormatList
		for _, format := range formats {
			if strings.Contains(format.MimeType, "audio/mp4") {
				audioFormats = append(audioFormats, format)
			}
		}
		if len(audioFormats) > 0 {
			audioFormat = &audioFormats[0]
		}

		if videoFormat == nil || audioFormat == nil {
			return fmt.Errorf("no suitable video or audio formats found for %s", info.Title)
		}
	} else {
		// For MP3: Get only audio format
		formats := video.Formats.WithAudioChannels()
		if len(formats) == 0 {
			return fmt.Errorf("no formats with audio found for %s", info.Title)
		}
		audioFormat = &formats[0]
	}

	if !d.config.MP3Only {
		// Download and merge video and audio
		videoStream, _, err := d.client.GetStream(video, videoFormat)
		if err != nil {
			return fmt.Errorf("failed to get video stream: %v", err)
		}
		defer videoStream.Close()

		audioStream, _, err := d.client.GetStream(video, audioFormat)
		if err != nil {
			return fmt.Errorf("failed to get audio stream: %v", err)
		}
		defer audioStream.Close()

		// Create temporary files for video and audio
		videoTempPath := tempPath + ".video"
		audioTempPath := tempPath + ".audio"

		// Download video stream
		if err := d.downloadStreamToFile(videoStream, videoTempPath, info.Title+" (video)"); err != nil {
			return err
		}

		// Download audio stream
		if err := d.downloadStreamToFile(audioStream, audioTempPath, info.Title+" (audio)"); err != nil {
			os.Remove(videoTempPath)
			return err
		}

		// Merge video and audio using ffmpeg
		if err := d.mergeVideoAudio(videoTempPath, audioTempPath, finalPath); err != nil {
			os.Remove(videoTempPath)
			os.Remove(audioTempPath)
			return err
		}

		// Clean up temporary files
		os.Remove(videoTempPath)
		os.Remove(audioTempPath)
	} else {
		// MP3 only download
		stream, _, err := d.client.GetStream(video, audioFormat)
		if err != nil {
			return fmt.Errorf("failed to get stream: %v", err)
		}
		defer stream.Close()

		if err := d.downloadStreamToFile(stream, tempPath, info.Title); err != nil {
			return err
		}

		if err := d.convertToMP3(tempPath, finalPath); err != nil {
			os.Remove(tempPath)
			return err
		}
		os.Remove(tempPath)
	}

	d.logger.Printf("Successfully downloaded: %s", info.Title)
	return nil
}

func (d *Downloader) ProcessPlaylist(playlistURL string) error {
	playlist, err := d.client.GetPlaylist(playlistURL)
	if err != nil {
		return fmt.Errorf("failed to get playlist: %v", err)
	}

	var wg sync.WaitGroup
	errors := make(chan error, len(playlist.Videos))

	for _, entry := range playlist.Videos {
		video, err := d.client.GetVideo(entry.ID)
		if err != nil {
			errors <- fmt.Errorf("failed to get video %s: %v", entry.ID, err)
			continue
		}

		wg.Add(1)
		go func(v *youtube.Video) {
			if err := d.downloadVideo(context.Background(), v, &wg); err != nil {
				errors <- err
			}
		}(video)
	}

	go func() {
		wg.Wait()
		close(errors)
	}()

	var downloadErrors []error
	for err := range errors {
		if err != nil {
			downloadErrors = append(downloadErrors, err)
		}
	}

	if len(downloadErrors) > 0 {
		return fmt.Errorf("encountered errors during download: %v", downloadErrors)
	}

	return nil
}

func (d *Downloader) downloadStreamToFile(stream io.Reader, filepath string, label string) error {
	out, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer out.Close()

	d.logger.Printf("Downloading %s", label)
	_, err = io.Copy(out, stream)
	return err
}

func (d *Downloader) mergeVideoAudio(videoPath, audioPath, outputPath string) error {
	d.logger.Printf("Merging video and audio streams...")
	cmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-i", audioPath,
		"-c:v", "copy",
		"-c:a", "aac",
		"-strict", "experimental",
		"-y",
		outputPath,
	)
	return cmd.Run()
}

func (d *Downloader) downloadWithProgress(stream io.Reader, out *os.File, size int64, title string) error {
	buffer := make([]byte, 1024)
	downloaded := int64(0)

	for {
		n, err := stream.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		_, err = out.Write(buffer[:n])
		if err != nil {
			return err
		}

		downloaded += int64(n)
		progress := float64(downloaded) / float64(size) * 100
		d.logger.Printf("\rDownloading %s: %.2f%%", title, progress)
	}

	return nil
}

func (d *Downloader) convertToMP3(inputPath, outputPath string) error {
	d.logger.Printf("Converting to MP3: %s", filepath.Base(outputPath))

	cmd := exec.Command("ffmpeg", "-i", inputPath, "-vn", "-ab", "128k", "-ar", "44100", "-y", outputPath)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %v", err)
	}

	return nil
}

func main() {
	mp3Flag := flag.Bool("mp3", false, "Download as MP3 (audio only)")
	outputDir := flag.String("output", "downloads", "Output directory")
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		fmt.Println("Usage: youtube-downloader [-mp3] [-output dir] <video_or_playlist_url>")
		os.Exit(1)
	}

	if *mp3Flag {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			log.Fatal("ffmpeg is required for MP3 conversion but it's not installed")
		}
	}

	config := Config{
		OutputDir:     *outputDir,
		MaxConcurrent: 3,
		Quality:       "best",
		MetadataOnly:  false,
		MP3Only:       *mp3Flag,
	}

	if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	downloader := NewDownloader(config)

	url := args[0]
	var err error

	if strings.Contains(url, "playlist?list=") {
		err = downloader.ProcessPlaylist(url)
	} else {
		video, err := downloader.client.GetVideo(url)
		if err != nil {
			log.Fatalf("Error getting video: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		err = downloader.downloadVideo(context.Background(), video, &wg)
		wg.Wait()
	}

	if err != nil {
		log.Fatalf("Error processing: %v", err)
	}

	fmt.Println("Download completed successfully!")
}
