package rpivaskuserquestion

// Ports view/props-adapter.ts (v2.15.0): the view fan-out. Two binding
// registries drive every component setter from canonical state: global bindings
// for the cross-tab components (dialog, submitPicker?, tabBar?), per-tab
// bindings for the per-tab kinds (optionList, preview, multiSelect?).

// BindingContext is the cross-tab projection context.
type BindingContext struct {
	Questions         []QuestionData
	ItemsByTab        [][]WrappingSelectItem
	TotalQuestions    int
	ActiveView        ActiveView
	InputBuffer       string
	InputCursorOffset *int
	ActivePreviewPane *PreviewPane
}

// PerTabBindingContext adds the per-tab fields.
type PerTabBindingContext struct {
	BindingContext
	Tab *TabComponents
	I   int
}

// ComponentBinding is one global binding spec.
type ComponentBinding struct {
	Component interface {
		SetProps(DialogProps)
	}
	Select func(state QuestionnaireState, ctx BindingContext) DialogProps
}

// BoundGlobalBinding is a type-erased global binding.
type BoundGlobalBinding struct {
	Apply      func(state QuestionnaireState, ctx BindingContext)
	Invalidate func()
}

// BoundPerTabBinding is a type-erased per-tab binding.
type BoundPerTabBinding struct {
	Apply func(state QuestionnaireState, ctx PerTabBindingContext)
}

// globalBinding mirrors globalBinding for the dialog (the only generic consumer
// the questionnaire wires with a per-component selector).
func globalBinding(component *DialogView, selector func(QuestionnaireState, BindingContext) DialogProps) BoundGlobalBinding {
	return BoundGlobalBinding{
		Apply:      func(state QuestionnaireState, ctx BindingContext) { component.SetProps(selector(state, ctx)) },
		Invalidate: component.Invalidate,
	}
}

// globalSubmitPickerBinding binds the submit picker.
func globalSubmitPickerBinding(component *SubmitPicker) BoundGlobalBinding {
	return BoundGlobalBinding{
		Apply: func(state QuestionnaireState, ctx BindingContext) {
			component.SetProps(SelectSubmitPickerProps(state, ctx))
		},
		Invalidate: component.Invalidate,
	}
}

// globalTabBarBinding binds the tab bar.
func globalTabBarBinding(component *TabBar) BoundGlobalBinding {
	return BoundGlobalBinding{
		Apply: func(state QuestionnaireState, ctx BindingContext) {
			component.SetProps(SelectTabBarProps(state, ctx))
		},
		Invalidate: component.Invalidate,
	}
}

// MultiSelectBinding applies the multi-select projection for a tab.
func MultiSelectBinding() BoundPerTabBinding {
	return BoundPerTabBinding{
		Apply: func(state QuestionnaireState, ctx PerTabBindingContext) {
			if ctx.Tab.MultiSelect == nil {
				return
			}
			ctx.Tab.MultiSelect.SetProps(SelectMultiSelectProps(state, ctx))
		},
	}
}

// OptionListBinding applies the option-list projection to the active tab.
func OptionListBinding() BoundPerTabBinding {
	return BoundPerTabBinding{
		Apply: func(state QuestionnaireState, ctx PerTabBindingContext) {
			if !IsActiveTab(state, ctx) {
				return
			}
			ctx.Tab.OptionList.SetProps(SelectOptionListProps(state, ctx))
		},
	}
}

// PreviewPaneBinding applies the preview projection to the active tab.
func PreviewPaneBinding() BoundPerTabBinding {
	return BoundPerTabBinding{
		Apply: func(state QuestionnaireState, ctx PerTabBindingContext) {
			if !IsActiveTab(state, ctx) {
				return
			}
			ctx.Tab.Preview.SetProps(SelectPreviewPaneProps(state, ctx))
		},
	}
}

// QuestionnairePropsAdapter drives every component setter from canonical state.
type QuestionnairePropsAdapter struct {
	questions           []QuestionData
	itemsByTab          [][]WrappingSelectItem
	tabsByIndex         []TabComponents
	inlineInput         *TextEditor
	globalBindings      []BoundGlobalBinding
	perTabBindings      []BoundPerTabBinding
	extraInvalidatables []Invalidatable
	requestRender       func()
}

// Invalidatable is the cache-invalidation contract.
type Invalidatable interface{ Invalidate() }

// QuestionnairePropsAdapterConfig is the adapter's construction-time config.
type QuestionnairePropsAdapterConfig struct {
	RequestRender       func()
	Questions           []QuestionData
	ItemsByTab          [][]WrappingSelectItem
	TabsByIndex         []TabComponents
	InlineInput         *TextEditor
	GlobalBindings      []BoundGlobalBinding
	PerTabBindings      []BoundPerTabBinding
	ExtraInvalidatables []Invalidatable
}

// NewQuestionnairePropsAdapter constructs the adapter.
func NewQuestionnairePropsAdapter(config QuestionnairePropsAdapterConfig) *QuestionnairePropsAdapter {
	return &QuestionnairePropsAdapter{
		questions:           config.Questions,
		itemsByTab:          config.ItemsByTab,
		tabsByIndex:         config.TabsByIndex,
		inlineInput:         config.InlineInput,
		globalBindings:      config.GlobalBindings,
		perTabBindings:      config.PerTabBindings,
		extraInvalidatables: config.ExtraInvalidatables,
		requestRender:       config.RequestRender,
	}
}

// Apply mirrors apply(): project canonical state onto every bound component,
// then request a render.
func (a *QuestionnairePropsAdapter) Apply(state QuestionnaireState) {
	if len(a.tabsByIndex) == 0 {
		return
	}
	totalQuestions := len(a.questions)
	activeView := SelectActiveView(state, totalQuestions)
	paneIndex := SelectActivePreviewPaneIndex(state.CurrentTab, totalQuestions)
	activePreviewPane := a.tabsByIndex[paneIndex].Preview
	if activePreviewPane == nil {
		activePreviewPane = a.tabsByIndex[0].Preview
	}
	offset := inputCursorOffset(a.inlineInput)

	ctx := BindingContext{
		Questions:         a.questions,
		ItemsByTab:        a.itemsByTab,
		TotalQuestions:    totalQuestions,
		ActiveView:        activeView,
		InputBuffer:       a.inlineInput.GetText(),
		InputCursorOffset: offset,
		ActivePreviewPane: activePreviewPane,
	}

	for _, binding := range a.globalBindings {
		binding.Apply(state, ctx)
	}
	for i := range a.tabsByIndex {
		tabCtx := PerTabBindingContext{BindingContext: ctx, Tab: &a.tabsByIndex[i], I: i}
		for _, binding := range a.perTabBindings {
			binding.Apply(state, tabCtx)
		}
	}
	if a.requestRender != nil {
		a.requestRender()
	}
}

// Invalidate mirrors invalidate(): every owned renderable, in registry order
// plus the extras.
func (a *QuestionnairePropsAdapter) Invalidate() {
	for _, binding := range a.globalBindings {
		if binding.Invalidate != nil {
			binding.Invalidate()
		}
	}
	for i := range a.tabsByIndex {
		a.tabsByIndex[i].OptionList.Invalidate()
		a.tabsByIndex[i].Preview.Invalidate()
		if a.tabsByIndex[i].MultiSelect != nil {
			a.tabsByIndex[i].MultiSelect.Invalidate()
		}
	}
	for _, extra := range a.extraInvalidatables {
		if extra != nil {
			extra.Invalidate()
		}
	}
}

// inputCursorOffset mirrors getInputCursorOffset: the cursor as a flat offset.
func inputCursorOffset(input *TextEditor) *int {
	if input == nil {
		return nil
	}
	lines := input.GetLines()
	cursor := input.GetCursor()
	offset := cursor.Col
	for i := 0; i < cursor.Line && i < len(lines); i++ {
		offset += len(lines[i]) + 1
	}
	return &offset
}
