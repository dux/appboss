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
				if key.Group != GroupHost {
					t.Errorf("host recipe %q field %q has group %s", recipe.ID, field.Path, key.Group)
				}
			case RecipeApp:
				if key.Group != GroupApp && key.Group != GroupShared {
					t.Errorf("app recipe %q field %q has group %s", recipe.ID, field.Path, key.Group)
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
		key     Key
		options []string
		want    string
	}{
		{key: Key{Type: "string"}, want: "text"},
		{key: Key{Type: "int"}, want: "number"},
		{key: Key{Type: "bool"}, want: "bool"},
		{key: Key{Type: "duration"}, want: "duration"},
		{key: Key{Type: "size"}, want: "size"},
		{key: Key{Type: "list"}, want: "list"},
		{key: Key{Type: "map"}, want: "map"},
		{key: Key{Type: "[from, to]"}, want: "range"},
		{key: Key{Type: "string"}, options: []string{"a"}, want: "select"},
		{key: Key{Type: "bool | button"}, options: []string{"true", "false", "button"}, want: "select"},
	} {
		if got := widgetKind(test.key, test.options); got != test.want {
			t.Errorf("widgetKind(%s, %v) = %s, want %s", test.key.Type, test.options, got, test.want)
		}
	}
}
