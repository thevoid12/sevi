package main

import (
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/billing"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// newBudget emails you when the project starts to cost money. Usage inside a free
// tier is billed at zero, so the budget's spend stays at $0.00 until something
// goes over; the first alert is at 1% of a $1 budget, i.e. one cent. The others
// are 50% and 100% of $1. No email address is configured: Google mails the
// billing account's admins and users by default. It only alerts, it never stops
// anything, and billing data lags by hours, so an alert is not instant.
// The budget covers this GCP project only (not R2, Neon or OpenRouter).
func newBudget(ctx *pulumi.Context, s settings, g *google) error {
	rule := func(percent float64) billing.BudgetThresholdRuleInput {
		return &billing.BudgetThresholdRuleArgs{ThresholdPercent: pulumi.Float64(percent)}
	}
	_, err := billing.NewBudget(ctx, "budget", &billing.BudgetArgs{
		BillingAccount: pulumi.String(s.BillingAccount),
		DisplayName:    pulumi.String("sevi: any paid usage"),
		BudgetFilter: &billing.BudgetBudgetFilterArgs{
			Projects: pulumi.StringArray{pulumi.Sprintf("projects/%s", g.project.Number)},
		},
		// No currency code: Google uses the billing account's own currency.
		Amount: &billing.BudgetAmountArgs{SpecifiedAmount: &billing.BudgetAmountSpecifiedAmountArgs{Units: pulumi.String("1")}},
		ThresholdRules: billing.BudgetThresholdRuleArray{
			rule(0.01), rule(0.5), rule(1),
		},
	}, pulumi.DependsOn(g.apis))
	return err
}
