package smugmug

import (
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Conf is the configuration of the smugmug worker
type Conf struct {
	ApiKey             string // API key
	ApiSecret          string // API secret
	UserToken          string // User token
	UserSecret         string // User secret
	Destination        string // Backup destination folder
	Filenames          string // Template for files naming
	FilenamesUnique    string // Template for alternate unique files naming
	UseMetadataTimes   bool   // When true, the last update timestamp will be retrieved from metadata
	ForceMetadataTimes bool   // When true, then the last update timestamp is always retrieved and overwritten, also for existing files

	username string
}

// overrideEnvConf overrides any configuration value if the
// corresponding environment variables is set
func (cfg *Conf) overrideEnvConf() {
	if os.Getenv("SMGMG_BK_API_KEY") != "" {
		cfg.ApiKey = os.Getenv("SMGMG_BK_API_KEY")
	}

	if os.Getenv("SMGMG_BK_API_SECRET") != "" {
		cfg.ApiSecret = os.Getenv("SMGMG_BK_API_SECRET")
	}

	if os.Getenv("SMGMG_BK_USER_TOKEN") != "" {
		cfg.UserToken = os.Getenv("SMGMG_BK_USER_TOKEN")
	}

	if os.Getenv("SMGMG_BK_USER_SECRET") != "" {
		cfg.UserSecret = os.Getenv("SMGMG_BK_USER_SECRET")
	}

	if os.Getenv("SMGMG_BK_DESTINATION") != "" {
		cfg.Destination = os.Getenv("SMGMG_BK_DESTINATION")
	}

	if os.Getenv("SMGMG_BK_FILE_NAMES") != "" {
		cfg.Filenames = os.Getenv("SMGMG_BK_FILE_NAMES")
	}
}

func (cfg *Conf) validate() error {
	if cfg.ApiKey == "" {
		return errors.New("ApiKey can't be empty")
	}

	if cfg.ApiSecret == "" {
		return errors.New("ApiSecret can't be empty")
	}

	if cfg.UserToken == "" {
		return errors.New("UserToken can't be empty")
	}

	if cfg.UserSecret == "" {
		return errors.New("UserSecret can't be empty")
	}

	if cfg.Destination == "" {
		return errors.New("Destination can't be empty")
	}

	// Check exising and writeability of destination folder
	if err := checkDestFolder(cfg.Destination); err != nil {
		return fmt.Errorf("Can't find in the destination folder %s: %v", cfg.Destination, err)
	}

	return nil
}

// ReadConf produces a configuration object for the Smugmug worker.
//
// It reads the configuration from ./config.toml or "$HOME/.smgmg/config.toml"
func ReadConf() (*Conf, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath("$HOME/.smgmg")
	viper.AddConfigPath(".")

	// defaults
	viper.SetDefault("store.file_names", "{{.FileName}}")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return nil, errors.New("Configuration file not found in ./config.toml or $HOME/.smgmg/config.toml")
		} else {
			return nil, err
		}
	}

	if viper.GetString("authentication.username") != "" {
		log.Warnf("[DEPRECATION] Username configuration value is ignored. It is now retrieved automatically from SmugMug based on the authentication credentials.")
	}

	cfg := &Conf{
		ApiKey:             viper.GetString("authentication.api_key"),
		ApiSecret:          viper.GetString("authentication.api_secret"),
		UserToken:          viper.GetString("authentication.user_token"),
		UserSecret:         viper.GetString("authentication.user_secret"),
		Destination:        viper.GetString("store.destination"),
		Filenames:          viper.GetString("store.file_names"),
		FilenamesUnique:    viper.GetString("store.file_names_unique"),
		UseMetadataTimes:   viper.GetBool("store.use_metadata_times"),
		ForceMetadataTimes: viper.GetBool("store.force_metadata_times"),
	}

	cfg.overrideEnvConf()

	if !cfg.UseMetadataTimes && cfg.ForceMetadataTimes {
		return nil, errors.New("Cannot use store.force_metadata_times without store.use_metadata_times")
	}

	return cfg, nil
}

// Worker actually implements the backup logic
type Worker struct {
	req          requestsHandler
	cfg          *Conf
	errors       int
	downloadFn   func(string, string, string, int64, string) (bool, error) // defined in struct for better testing
	filenameTmpl *template.Template
	filenameUniqueTmpl *template.Template
}

// New return a SmugMug backup configuration. It returns an error if it fails parsing
// the command line arguments
func New(cfg *Conf) (*Worker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	handler := newHTTPHandler(cfg.ApiKey, cfg.ApiSecret, cfg.UserToken, cfg.UserSecret)

	tmpl, err := buildFilenameTemplate(cfg.Filenames)
	tmplUnique, err := buildUniqueFilenameTemplate(cfg.FilenamesUnique)
	if err != nil {
		return nil, err
	}

	return &Worker{
		cfg:          cfg,
		req:          handler,
		downloadFn:   handler.download,
		filenameTmpl: tmpl,
		filenameUniqueTmpl: tmplUnique,
	}, nil
}

func buildFilenameTemplate(filenameTemplate string) (*template.Template, error) {
	// Use FileName as default
	if filenameTemplate == "" {
		filenameTemplate = "{{.FileName}}"
	}

	tmpl, err := template.New("image_filename").Option("missingkey=error").Parse(filenameTemplate)
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}

func buildUniqueFilenameTemplate(filenameTemplate string) (*template.Template, error) {
	// Use FileName with ImageKey appended as default
	if filenameTemplate == "" {
		filenameTemplate = "{{appendFileName .FileName .ImageKey}}"
	}

	var fns = template.FuncMap{
		"appendFileName": func(fileName string, imageKey string) string{
			fileExtLoc := strings.LastIndex(fileName, ".")
			prefix := fileName[:fileExtLoc]
			extension := fileName[fileExtLoc:len(fileName)]
			result := prefix + imageKey + extension
			return result
		},
	}

	tmpl, err := template.New("image_filename_unique").Funcs(fns).Option("missingkey=error").Parse(filenameTemplate)
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}

// Run performs the backup of the provided SmugMug account.
//
// The workflow is the following:
//
//   - Get user albums
//   - Iterate over all albums and:
//     - create folder
//     - iterate over all images and videos
//       - if existing and with the same size, then skip
//       - if not, download
func (w *Worker) Run() error {
	var err error
	w.cfg.username, err = w.currentUser()
	if err != nil {
		return fmt.Errorf("Error checking credentials: %v", err)
	}

	// Get user albums
	log.Infof("Getting albums for user %s...\n", w.cfg.username)
	albums, err := w.userAlbums()
	if err != nil {
		return fmt.Errorf("Error getting user albums: %v", err)
	}

	log.Infof("Found %d albums\n", len(albums))

	for _, album := range albums {
		folder := filepath.Join(w.cfg.Destination, album.URLPath)

		if err := createFolder(folder); err != nil {
			log.WithError(err).Errorf("cannot create the destination folder %s", folder)
			w.errors++
			continue
		}

		log.Debugf("[ALBUM IMAGES] %s", album.Uris.AlbumImages.URI)
		images, err := w.albumImages(album.Uris.AlbumImages.URI, album.URLPath)
		if err != nil {
			log.WithError(err).Errorf("Cannot get album images for %s", album.Uris.AlbumImages.URI)
			w.errors++
			continue
		}

		log.Debugf("Got album images for %s", album.Uris.AlbumImages.URI)
		log.Debugf("%+v", images)
		w.saveImages(images, folder)
	}

	if w.errors > 0 {
		return fmt.Errorf("Completed with %d errors, please check logs", w.errors)
	}

	log.Info("Backup completed.")
	return nil
}
