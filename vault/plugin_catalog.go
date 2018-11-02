package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/vault/helper/consts"
	"github.com/hashicorp/vault/helper/jsonutil"
	"github.com/hashicorp/vault/helper/pluginutil"
	"github.com/hashicorp/vault/logical"
)

var (
	pluginCatalogPath         = "core/plugin-catalog/"
	ErrDirectoryNotConfigured = errors.New("could not set plugin, plugin directory is not configured")
	ErrPluginNotFound         = errors.New("plugin not found in the catalog")
)

// PluginCatalog keeps a record of plugins known to vault. External plugins need
// to be registered to the catalog before they can be used in backends. Builtin
// plugins are automatically detected and included in the catalog.
type PluginCatalog struct {
	builtinRegistry BuiltinRegistry
	catalogView     *BarrierView
	directory       string

	lock sync.RWMutex
}

func (c *Core) setupPluginCatalog() error {
	c.pluginCatalog = &PluginCatalog{
		builtinRegistry: c.builtinRegistry,
		catalogView:     NewBarrierView(c.barrier, pluginCatalogPath),
		directory:       c.pluginDirectory,
	}

	if c.logger.IsInfo() {
		c.logger.Info("successfully setup plugin catalog", "plugin-directory", c.pluginDirectory)
	}

	return nil
}

// Get retrieves a plugin with the specified name from the catalog. It first
// looks for external plugins with this name and then looks for builtin plugins.
// It returns a PluginRunner or an error if no plugin was found.
func (c *PluginCatalog) Get(ctx context.Context, name string, pluginType consts.PluginType) (*pluginutil.PluginRunner, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	// If the directory isn't set only look for builtin plugins.
	if c.directory != "" {
		// Look for external plugins in the barrier
		out, err := c.catalogView.Get(ctx, pluginType.String()+"-"+name)
		if err != nil {
			return nil, errwrap.Wrapf(fmt.Sprintf("failed to retrieve plugin %q: {{err}}", name), err)
		}
		if out == nil {
			// Also look for external plugins under what their name would have been if they
			// were registered before plugin types existed.
			out, err = c.catalogView.Get(ctx, name)
			if err != nil {
				return nil, errwrap.Wrapf(fmt.Sprintf("failed to retrieve plugin %q: {{err}}", name), err)
			}
		}
		if out != nil {
			entry := new(pluginutil.PluginRunner)
			if err := jsonutil.DecodeJSON(out.Value, entry); err != nil {
				return nil, errwrap.Wrapf("failed to decode plugin entry: {{err}}", err)
			}
			if entry.Type != pluginType && entry.Type != consts.PluginTypeUnknown {
				return nil, nil
			}

			// prepend the plugin directory to the command
			entry.Command = filepath.Join(c.directory, entry.Command)

			return entry, nil
		}
	}
	// Look for builtin plugins
	if factory, ok := c.builtinRegistry.Get(name, pluginType); ok {
		return &pluginutil.PluginRunner{
			Name:           name,
			Type:           pluginType,
			Builtin:        true,
			BuiltinFactory: factory,
		}, nil
	}

	return nil, nil
}

// Set registers a new external plugin with the catalog, or updates an existing
// external plugin. It takes the name, command and SHA256 of the plugin.
func (c *PluginCatalog) Set(ctx context.Context, name string, pluginType consts.PluginType, command string, args []string, env []string, sha256 []byte) error {
	if c.directory == "" {
		return ErrDirectoryNotConfigured
	}

	switch {
	case strings.Contains(name, ".."):
		fallthrough
	case strings.Contains(command, ".."):
		return consts.ErrPathContainsParentReferences
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	// Best effort check to make sure the command isn't breaking out of the
	// configured plugin directory.
	commandFull := filepath.Join(c.directory, command)
	sym, err := filepath.EvalSymlinks(commandFull)
	if err != nil {
		return errwrap.Wrapf("error while validating the command path: {{err}}", err)
	}
	symAbs, err := filepath.Abs(filepath.Dir(sym))
	if err != nil {
		return errwrap.Wrapf("error while validating the command path: {{err}}", err)
	}

	if symAbs != c.directory {
		return errors.New("can not execute files outside of configured plugin directory")
	}

	entry := &pluginutil.PluginRunner{
		Name:    name,
		Type:    pluginType,
		Command: command,
		Args:    args,
		Env:     env,
		Sha256:  sha256,
		Builtin: false,
	}

	buf, err := json.Marshal(entry)
	if err != nil {
		return errwrap.Wrapf("failed to encode plugin entry: {{err}}", err)
	}

	logicalEntry := logical.StorageEntry{
		Key:   pluginType.String() + "-" + name,
		Value: buf,
	}
	if err := c.catalogView.Put(ctx, &logicalEntry); err != nil {
		return errwrap.Wrapf("failed to persist plugin entry: {{err}}", err)
	}
	return nil
}

// Delete is used to remove an external plugin from the catalog. Builtin plugins
// can not be deleted.
func (c *PluginCatalog) Delete(ctx context.Context, name string, pluginType consts.PluginType) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Check the name under which the plugin exists, but if it's unfound, don't return any error.
	pluginKey := pluginType.String() + "-" + name
	out, err := c.catalogView.Get(ctx, pluginKey)
	if err != nil || out == nil {
		pluginKey = name
	}

	return c.catalogView.Delete(ctx, pluginKey)
}

// List returns a list of all the known plugin names. If an external and builtin
// plugin share the same name, only one instance of the name will be returned.
func (c *PluginCatalog) List(ctx context.Context, pluginType consts.PluginType) ([]string, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	// Collect keys for external plugins in the barrier.
	keys, err := logical.CollectKeys(ctx, c.catalogView)
	if err != nil {
		return nil, err
	}

	// Get the builtin plugins.
	builtinKeys := c.builtinRegistry.Keys(pluginType)

	// Use a map to unique the two lists.
	mapKeys := make(map[string]bool)

	pluginTypePrefix := pluginType.String() + "-"

	for _, plugin := range keys {

		// Only list user-added plugins if they're of the given type.
		if entry, err := c.Get(ctx, plugin, pluginType); err == nil && entry != nil {

			// Some keys will be prepended with the plugin type, but other ones won't.
			// Users don't expect to see the plugin type, so we need to strip that here.
			idx := strings.Index(plugin, pluginTypePrefix)
			if idx == 0 {
				plugin = plugin[len(pluginTypePrefix):]
			}
			mapKeys[plugin] = true
		}
	}

	for _, plugin := range builtinKeys {
		mapKeys[plugin] = true
	}

	retList := make([]string, len(mapKeys))
	i := 0
	for k := range mapKeys {
		retList[i] = k
		i++
	}
	// sort for consistent ordering of builtin plugins
	sort.Strings(retList)

	return retList, nil
}
