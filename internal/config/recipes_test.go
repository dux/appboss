package config

import "testing"

func TestRecipesReferenceRealKeys(t *testing.T) {
	keys := map[string]Key{}
	for _, key := range Keys() {
		keys[key.Path] = key
	}
	seenRecipe := map[string]bool{}
	for _, recipe := range Recipes() {
		if recipe.ID == "" || recipe.Title == "" || recipe.Description == "" {
			t.Errorf("recipe %q is missing id, title or description", recipe.ID)
		}
		if recipe.Scope != RecipeHost && recipe.Scope != RecipeApp {
			t.Errorf("recipe %q has invalid scope %q", recipe.ID, recipe.Scope)
		}
		if seenRecipe[recipe.ID] {
			t.Errorf("recipe %q is listed twice", recipe.ID)
		}
		seenRecipe[recipe.ID] = true
		if len(recipe.Fields) == 0 {
			t.Errorf("recipe %q has no fields", recipe.ID)
		}
		seenField := map[string]bool{}
		for _, field := range recipe.Fields {
			key, ok := keys[field.Path]
			if !ok {
				t.Errorf("recipe %q references unknown key %q", recipe.ID, field.Path)
				continue
			}
			if seenField[field.Path] {
				t.Errorf("recipe %q lists %q twice", recipe.ID, field.Path)
			}
			seenField[field.Path] = true
			if field.Label == "" || field.Description == "" || field.Kind == "" {
				t.Errorf("recipe %q field %q is missing label, description or kind", recipe.ID, field.Path)
			}
			if field.Type != key.Type || field.Default != key.Default || field.Example != key.Example {
				t.Errorf("recipe %q field %q drifts from Keys: %+v vs %+v", recipe.ID, field.Path, field, key)
			}
			switch recipe.Scope {
			case RecipeHost:
				if key.Scope != ScopeService {
					t.Errorf("host recipe %q field %q has scope %s", recipe.ID, field.Path, key.Scope)
				}
			case RecipeApp:
				if key.Scope != ScopeApp && key.Scope != ScopeBoth {
					t.Errorf("app recipe %q field %q has scope %s", recipe.ID, field.Path, key.Scope)
				}
			}
			if len(field.Options) > 0 && field.Kind != "select" {
				t.Errorf("recipe %q field %q has options but kind %q", recipe.ID, field.Path, field.Kind)
			}
		}
	}
}

func TestWidgetKind(t *testing.T) {
	for _, test := range []struct {
		key  Key
		want string
	}{
		{key: Key{Types: []string{"string"}}, want: "text"},
		{key: Key{Types: []string{"int"}}, want: "number"},
		{key: Key{Types: []string{"bool"}}, want: "bool"},
		{key: Key{Types: []string{"duration"}}, want: "duration"},
		{key: Key{Types: []string{"size"}}, want: "size"},
		{key: Key{Types: []string{"list"}}, want: "list"},
		{key: Key{Types: []string{"string", "list"}}, want: "list"},
		{key: Key{Types: []string{"map"}}, want: "map"},
		{key: Key{Types: []string{"[from, to]"}}, want: "range"},
		{key: Key{Types: []string{"string"}, Enum: []string{"a"}}, want: "select"},
		{key: Key{Types: []string{"bool", "button"}, Enum: []string{"true", "false", "button"}}, want: "select"},
	} {
		if got := widgetKind(test.key); got != test.want {
			t.Errorf("widgetKind(%v, %v) = %s, want %s", test.key.Types, test.key.Enum, got, test.want)
		}
	}
}
