package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

type positionPayout struct {
	Payout       sdk.Dec
	PnlNotional  sdk.Dec
	IsProfitable bool
}

func (m *Position) IsShort() bool { return !m.IsLong }

func (m *PositionDelta) IsShort() bool { return !m.IsLong }

// NewPosition initializes a new position with a given cumulativeFundingEntry (should be nil for non-perpetual markets)
func NewPosition(isLong bool, cumulativeFundingEntry sdk.Dec) *Position {
	position := &Position{
		IsLong:       isLong,
		Quantity:     sdk.ZeroDec(),
		EntryPrice:   sdk.ZeroDec(),
		Margin:       sdk.ZeroDec(),
		HoldQuantity: sdk.ZeroDec(),
	}
	if !cumulativeFundingEntry.IsNil() {
		position.CumulativeFundingEntry = cumulativeFundingEntry
	}
	return position
}

// ApplyProfitHaircut results in reducing the payout (pnl * quantity) by the given rate (e.g. 0.1=10%) by modifying the entry price.
func (p *Position) ApplyProfitHaircut(haircutPercentage, settlementPrice sdk.Dec) {
	// newEntryPrice = entryPrice - entryPrice * haircutPercentage + settlePrice * haircutPercentage
	newEntryPrice := p.EntryPrice.Sub(p.EntryPrice.Mul(haircutPercentage)).Add(settlementPrice.Mul(haircutPercentage))
	p.EntryPrice = newEntryPrice
}

func (p *Position) ClosePositionWithSettlePrice(settlePrice sdk.Dec) sdk.Dec {
	// TODO change this, there should not be positions with 0 quantity
	if p.Quantity.IsZero() {
		return sdk.ZeroDec()
	}

	closingDirection := !p.IsLong
	fullyClosingQuantity := p.Quantity

	positionDelta := &PositionDelta{
		IsLong:            closingDirection,
		ExecutionQuantity: fullyClosingQuantity,
		ExecutionMargin:   sdk.ZeroDec(),
		ExecutionPrice:    settlePrice,
	}

	payout, _, _, _ := p.ApplyPositionDelta(positionDelta, sdk.ZeroDec())
	return payout
}

func (p *Position) GetDirectionString() string {
	directionStr := "Long"
	if p.IsShort() {
		directionStr = "Short"
	}
	return directionStr
}

func (p *Position) CheckValidPositionToSell(order OrderInfo, isBuyOrder bool, tradeFeeRate sdk.Dec, funding *PerpetualMarketFunding) error {

	if order.Quantity.GT(p.Quantity.Sub(p.HoldQuantity)) {
		return sdkerrors.Wrapf(ErrInsufficientPositionQuantity, "Position Quantity %s - Hold Quantity %s must be GTE reduce-only order quantity %s", p.Quantity.String(), p.HoldQuantity.String(), order.Quantity.String())
	}

	if isBuyOrder == p.IsLong {
		return ErrInvalidReduceOnlyPositionDirection
	}

	if err := p.CheckValidClosingPrice(order.Price, tradeFeeRate, funding); err != nil {
		return err
	}

	return nil
}

func (p *Position) CheckValidClosingPrice(closingPrice sdk.Dec, tradeFeeRate sdk.Dec, funding *PerpetualMarketFunding) error {
	bankruptcyPrice := p.GetBankruptcyPrice(funding)

	if p.IsLong {
		// For long positions, Price ≥ BankruptcyPrice / (1 - TakerFeeRate) must hold
		feeAdjustedBankruptcyPrice := bankruptcyPrice.Quo(sdk.OneDec().Sub(tradeFeeRate))

		if !closingPrice.GTE(feeAdjustedBankruptcyPrice) {
			return ErrPriceSurpassesBankruptcyPrice
		}
	} else {
		// For short positions, Price ≤ BankruptcyPrice / (1 + TakerFeeRate) must hold
		feeAdjustedBankruptcyPrice := bankruptcyPrice.Quo(sdk.OneDec().Add(tradeFeeRate))

		if !closingPrice.LTE(feeAdjustedBankruptcyPrice) {
			return ErrPriceSurpassesBankruptcyPrice
		}
	}
	return nil
}

func (p *Position) GetBankruptcyPrice(funding *PerpetualMarketFunding) (bankruptcyPrice sdk.Dec) {
	return p.GetLiquidationPrice(sdk.ZeroDec(), funding)
}

func (p *Position) GetLiquidationPrice(maintenanceMarginRatio sdk.Dec, funding *PerpetualMarketFunding) sdk.Dec {
	adjustedUnitMargin := p.getFundingAdjustedUnitMargin(funding)

	var liquidationPrice sdk.Dec
	if p.IsLong {
		// liquidation price = (entry price - unit margin) / (1 - maintenanceMarginRatio)
		liquidationPrice = p.EntryPrice.Sub(adjustedUnitMargin).Quo(sdk.OneDec().Sub(maintenanceMarginRatio))
	} else {
		// liquidation price = (entry price + unit margin) / (1 + maintenanceMarginRatio)
		liquidationPrice = p.EntryPrice.Add(adjustedUnitMargin).Quo(sdk.OneDec().Add(maintenanceMarginRatio))
	}
	return liquidationPrice
}

func (p *Position) getFundingAdjustedUnitMargin(funding *PerpetualMarketFunding) sdk.Dec {
	adjustedMargin := p.Margin

	// Compute the adjusted position margin for positions in perpetual markets
	if funding != nil {
		unrealizedFundingPayment := p.Quantity.Mul(p.CumulativeFundingEntry.Sub(funding.CumulativeFunding))

		// For longs, Margin -= Funding
		// For shorts, Margin += Funding
		if p.IsLong {
			adjustedMargin = p.Margin.Sub(unrealizedFundingPayment)
		} else {
			adjustedMargin = p.Margin.Add(unrealizedFundingPayment)
		}
	}

	// Unit Margin = PositionMargin / PositionQuantity
	fundingAdjustedUnitMargin := adjustedMargin.Quo(p.Quantity)
	return fundingAdjustedUnitMargin
}

// ApplyFundingAndGetUpdatedPositionState updates the position to account for any funding payment and returns a PositionState.
func (p *Position) ApplyFundingAndGetUpdatedPositionState(funding *PerpetualMarketFunding) *PositionState {
	fundingPayment := sdk.ZeroDec()

	if funding != nil {
		fundingPayment = p.Quantity.Mul(p.CumulativeFundingEntry.Sub(funding.CumulativeFunding))
		// For longs, Margin -= Funding
		// For shorts, Margin += Funding
		if p.IsLong {
			fundingPayment = fundingPayment.Neg()
		}
		p.Margin = p.Margin.Add(fundingPayment)
	}
	positionState := PositionState{
		Position:       p,
		FundingPayment: fundingPayment,
	}
	return &positionState
}

func (p *Position) GetAverageWeightedEntryPrice(executionQuantity, executionPrice sdk.Dec) sdk.Dec {
	num := p.Quantity.Mul(p.EntryPrice).Add(executionQuantity.Mul(executionPrice))
	denom := p.Quantity.Add(executionQuantity)

	return num.Quo(denom)
}

func (p *Position) GetPayoutIfFullyClosing(closingPrice, tradeFeeRate sdk.Dec) *positionPayout {
	isProfitable := (p.IsLong && p.EntryPrice.LT(closingPrice)) || (!p.IsLong && p.EntryPrice.GT(closingPrice))

	fullyClosingQuantity := p.Quantity
	positionClosingMargin := p.Margin

	closeTradingFee := closingPrice.Mul(fullyClosingQuantity).Mul(tradeFeeRate)
	payoutFromPnl := p.GetPayoutFromPnl(closingPrice, fullyClosingQuantity)
	pnlNotional := payoutFromPnl.Sub(closeTradingFee)
	payout := pnlNotional.Add(positionClosingMargin)

	return &positionPayout{
		Payout:       payout,
		PnlNotional:  pnlNotional,
		IsProfitable: isProfitable,
	}
}

func (p *Position) GetPayoutFromPnl(closingPrice, closingQuantity sdk.Dec) sdk.Dec {
	var pnlNotional sdk.Dec

	if p.IsLong {
		// pnl = closingQuantity * (executionPrice - entryPrice)
		pnlNotional = closingQuantity.Mul(closingPrice.Sub(p.EntryPrice))
	} else {
		// pnl = -closingQuantity * (executionPrice - entryPrice)
		pnlNotional = closingQuantity.Mul(closingPrice.Sub(p.EntryPrice)).Neg()
	}

	return pnlNotional
}

func (p *Position) ApplyPositionDelta(delta *PositionDelta, tradingFeeForReduceOnly sdk.Dec) (
	payout, pnlNotional, closeExecutionMargin, collateralizationMargin sdk.Dec,
) {
	// No payouts or margin changes if the position delta is nil
	if delta == nil || p == nil {
		return sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec()
	}

	if p.Quantity.IsZero() {
		p.IsLong = delta.IsLong
	}

	payout, pnlNotional, closeExecutionMargin, collateralizationMargin = sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec()
	isNettingInSameDirection := (p.IsLong && delta.IsLong) || (p.IsShort() && delta.IsShort())

	if isNettingInSameDirection {
		p.EntryPrice = p.GetAverageWeightedEntryPrice(delta.ExecutionQuantity, delta.ExecutionPrice)
		p.Quantity = p.Quantity.Add(delta.ExecutionQuantity)
		p.Margin = p.Margin.Add(delta.ExecutionMargin)
		collateralizationMargin = delta.ExecutionMargin

		return payout, pnlNotional, closeExecutionMargin, collateralizationMargin
	}

	// netting in opposing direction
	closingQuantity := sdk.MinDec(p.Quantity, delta.ExecutionQuantity)
	// closeExecutionMargin = execution margin * closing quantity / execution quantity
	closeExecutionMargin = delta.ExecutionMargin.Mul(closingQuantity).Quo(delta.ExecutionQuantity)

	if p.IsLong {
		// pnl = closingQuantity * (executionPrice - entryPrice)
		pnlNotional = closingQuantity.Mul(delta.ExecutionPrice.Sub(p.EntryPrice))
	} else {
		// pnl = -closingQuantity * (executionPrice - entryPrice)
		pnlNotional = closingQuantity.Mul(delta.ExecutionPrice.Sub(p.EntryPrice)).Neg()
	}

	payoutFromPnl := p.GetPayoutFromPnl(delta.ExecutionPrice, closingQuantity)
	isReduceOnlyTrade := delta.ExecutionMargin.IsZero()

	if isReduceOnlyTrade {
		// deduct fees from PNL (position margin) for reduce-only orders

		// only use the closing trading fee for now
		pnlNotional = payoutFromPnl.Sub(tradingFeeForReduceOnly)
		p.HoldQuantity = p.HoldQuantity.Sub(closingQuantity)
	}

	positionClosingMargin := p.Margin.Mul(closingQuantity).Quo(p.Quantity)
	payout = pnlNotional.Add(positionClosingMargin)

	// for netting opposite direction
	newPositionQuantity := p.Quantity.Sub(closingQuantity)
	p.Margin = p.Margin.Mul(newPositionQuantity).Quo(p.Quantity)
	p.Quantity = newPositionQuantity

	isFlippingPosition := delta.ExecutionQuantity.GT(closingQuantity)

	if isFlippingPosition {
		remainingExecutionQuantity := delta.ExecutionQuantity.Sub(closingQuantity)
		remainingExecutionMargin := delta.ExecutionMargin.Sub(closeExecutionMargin)
		newPositionDelta := &PositionDelta{
			IsLong:            !p.IsLong,
			ExecutionQuantity: remainingExecutionQuantity,
			ExecutionMargin:   remainingExecutionMargin,
			ExecutionPrice:    delta.ExecutionPrice,
		}
		// recurse
		_, _, _, collateralizationMargin = p.ApplyPositionDelta(newPositionDelta, tradingFeeForReduceOnly)
	}

	return payout, pnlNotional, closeExecutionMargin, collateralizationMargin
}
