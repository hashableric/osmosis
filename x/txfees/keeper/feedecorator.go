package keeper

import (
	"errors"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	gammtypes "github.com/osmosis-labs/osmosis/v7/x/gamm/types"
	"github.com/osmosis-labs/osmosis/v7/x/txfees/keeper/txfee_filters"
	"github.com/osmosis-labs/osmosis/v7/x/txfees/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

// MempoolFeeDecorator will check if the transaction's fee is at least as large
// as the local validator's minimum gasFee (defined in validator config).
// If fee is too low, decorator returns error and tx is rejected from mempool.
// Note this only applies when ctx.CheckTx = true
// If fee is high enough or not CheckTx, then call next AnteHandler
// CONTRACT: Tx must implement FeeTx to use MempoolFeeDecorator.
type MempoolFeeDecorator struct {
	TxFeesKeeper Keeper
	Opts         types.MempoolFeeOptions
}

func NewMempoolFeeDecorator(txFeesKeeper Keeper, opts types.MempoolFeeOptions) MempoolFeeDecorator {
	return MempoolFeeDecorator{
		TxFeesKeeper: txFeesKeeper,
		Opts:         opts,
	}
}

func (mfd MempoolFeeDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (newCtx sdk.Context, err error) {
	// The SDK currently requires all txs to be FeeTx's in CheckTx, within its mempool fee decorator.
	// See: https://github.com/cosmos/cosmos-sdk/blob/f726a2398a26bdaf71d78dbf56a82621e84fd098/x/auth/middleware/fee.go#L34-L37
	// So this is not a real restriction at the moment.
	feeTx, ok := tx.(sdk.FeeTx)
	if !ok {
		return ctx, sdkerrors.Wrap(sdkerrors.ErrTxDecode, "Tx must be a FeeTx")
	}

	// Ensure that the provided gas is less than the maximum gas per tx,
	// if this is a CheckTx. This is only for local mempool purposes, and thus
	// is only ran on check tx.
	if ctx.IsCheckTx() && !simulate {
		if feeTx.GetGas() > mfd.Opts.MaxGasWantedPerTx {
			msg := "Too much gas wanted: %d, maximum is %d"
			return ctx, sdkerrors.Wrapf(sdkerrors.ErrOutOfGas, msg, feeTx.GetGas(), mfd.Opts.MaxGasWantedPerTx)
		}
	}

	feeCoins := feeTx.GetFee()

	if len(feeCoins) > 1 {
		return ctx, types.ErrTooManyFeeCoins
	}

	baseDenom, err := mfd.TxFeesKeeper.GetBaseDenom(ctx)
	if err != nil {
		return ctx, err
	}

	// If there is a fee attached to the tx, make sure the fee denom is a denom accepted by the chain
	if len(feeCoins) == 1 {
		feeDenom := feeCoins.GetDenomByIndex(0)
		if feeDenom != baseDenom {
			_, err := mfd.TxFeesKeeper.GetFeeToken(ctx, feeDenom)
			if err != nil {
				return ctx, err
			}
		}
	}

	// If we are in CheckTx, this function is ran locally to determine if these fees are sufficient
	// to enter our mempool.
	// So we ensure that the provided fees meet a minimum threshold for the validator,
	// converting every non-osmo specified asset into an osmo-equivalent amount, to determine sufficiency.
	if (ctx.IsCheckTx() || ctx.IsReCheckTx()) && !simulate {
		minBaseGasPrice := mfd.GetMinBaseGasPriceForTx(ctx, baseDenom, feeTx)
		if !(minBaseGasPrice.IsZero()) {
			if len(feeCoins) != 1 {
				return ctx, sdkerrors.Wrapf(sdkerrors.ErrInsufficientFee, "no fee attached")
			}
			err = mfd.TxFeesKeeper.IsSufficientFee(ctx, minBaseGasPrice, feeTx.GetGas(), feeCoins[0], tx.GetMsgs()[0])
			if err != nil {
				return ctx, err
			}
		}
	}

	return next(ctx, tx, simulate)
}

// getFeeTokenAmountFromSwapMsg determines which tryp of swap message is passed and returns the token amount in/out
func getFeeTokenAmountFromSwapMsg(msg gammtypes.SwapMsgRoute, firstDenom string) (sdk.Coin, error) {
	if _, ok := msg.(gammtypes.SwapMsgRoute); !ok {
		return sdk.Coin{}, errors.New("SwapMsgRoute msg neither MsgSwapExactAmountOut nor MsgSwapExactAmountIn")
	}

	msgIn, ok := (msg.(gammtypes.SwapExactIn))
	if !ok {
		msgOut, ok := (msg.(gammtypes.SwapExactOut))
		if !ok {
			return sdk.Coin{}, errors.New("SwapMsgRoute msg neither MsgSwapExactAmount nor  MsgSwapExactAmountIn")
		} else {
			// MsgSwapExactAmountOut ==> fee is paid in the amount in
			amount := msgOut.GetTokenAmountIn()
			return sdk.NewCoin(msg.TokenInDenom(), amount), nil
		}
	} else {
		// MsgSwapExactAmountIn ==> fee is pain the amount out
		amount := msgIn.GetTokenAmountOut()
		return sdk.NewCoin(msg.TokenOutDenom(), amount), nil
	}
}

// get swapFeesSybilResistantlySpent returns the amount
func (k Keeper) getSwapFeesSybilResitantlySpent(ctx sdk.Context, msg gammtypes.SwapMsgRoute) sdk.Coin {
	// msgs is a SwapMsgRoute. Get PoolIds on the route
	denoms, poolIds := msg.TokenDenomsOnPath()
	var swapFees sdk.Dec
	for i := 0; i < len(poolIds); i++ {
		swapFee, err := k.gammKeeper.GetSwapFeeForSybilResistance(ctx, poolIds[i])
		if err != nil {
			// TODO: handle err - right now GetMinBaseGasPriceForTx does not return an error
			return sdk.Coin{}
		}

		swapFees.Add(swapFee)
	}
	if swapFees.IsZero() {
		return sdk.Coin{}
	}

	msgCoin, err := getFeeTokenAmountFromSwapMsg(msg, denoms[0])
	if err != nil {
		// SwapMsgRoute incorrectly cast - no fee reduction
		return sdk.Coin{}
	}
	swapFeesResistantlySpent := swapFees.MulInt(msgCoin.Amount)
	feesPaid, _ := k.ConvertToBaseToken(ctx, sdk.NewCoin(msgCoin.Denom, swapFeesResistantlySpent.RoundInt()))
	//if err != nil {
	// TODO: handle error
	//	return sdk.Dec{}
	//}
	return feesPaid
}

// IsSufficientFee checks if the feeCoin provided (in any asset), is worth enough osmo at current spot prices
// to pay the gas cost of this tx.
func (k Keeper) IsSufficientFee(ctx sdk.Context, minBaseGasPrice sdk.Dec, gasRequested uint64, feeCoin sdk.Coin, msg sdk.Msg) error {
	baseDenom, err := k.GetBaseDenom(ctx)
	if err != nil {
		return err
	}
	// get sybil resitantly spent fees
	var feesSybilResistantlySpent sdk.Coin
	// force type check the msg to be a swap msg
	msgSwap, ok := msg.(gammtypes.SwapMsgRoute)
	if !ok {
		// not a swap - feesSybilResistantlySpent is zero at the base denom
		feesSybilResistantlySpent.Amount = sdk.ZeroInt()
		feesSybilResistantlySpent.Denom = baseDenom
	} else {
		// is a swap - feesSybilResistantlySpent is determed as the
		// (sum of the fees)*(amountIn/Out) converted to the base denom
		feesSybilResistantlySpent = k.getSwapFeesSybilResitantlySpent(ctx, msgSwap)
		if !feesSybilResistantlySpent.Amount.GT(sdk.ZeroInt()) {
			// In the case of a erroneous swap casting, the coin needs to be 0 of baseDenom
			feesSybilResistantlySpent.Amount = sdk.ZeroInt()
			feesSybilResistantlySpent.Denom = baseDenom
		}
	}
	// Determine the required fees by multiplying the required minimum gas
	// price by the gas limit, where fee = ceil(minGasPrice * gasLimit).
	glDec := sdk.NewDec(int64(gasRequested))
	requiredBaseFee := sdk.NewCoin(baseDenom, minBaseGasPrice.Mul(glDec).Ceil().RoundInt())

	convertedFee, err := k.ConvertToBaseToken(ctx, feeCoin.Sub(feesSybilResistantlySpent))
	if err != nil {
		return err
	}
	convertedFeeSybilReduced := convertedFee.Sub(feesSybilResistantlySpent)
	// instead of using the subtracted converted fee, we reduce the requiredBaseFee.
	// This prevents the converted fee from being made lower than the required base fee when it otherwise isn't.
	// However, the error will still return the feesSybilResistantlySpent reduced converted fee if the error occurs
	if !(convertedFee.IsGTE(requiredBaseFee.Sub(feesSybilResistantlySpent))) {
		return sdkerrors.Wrapf(sdkerrors.ErrInsufficientFee, "insufficient fees; got: %s which converts to %s. required: %s", feeCoin, convertedFeeSybilReduced, requiredBaseFee)
	}

	return nil
}

func (mfd MempoolFeeDecorator) GetMinBaseGasPriceForTx(ctx sdk.Context, baseDenom string, tx sdk.FeeTx) sdk.Dec {
	cfgMinGasPrice := ctx.MinGasPrices().AmountOf(baseDenom)

	if tx.GetGas() >= mfd.Opts.HighGasTxThreshold {
		cfgMinGasPrice = sdk.MaxDec(cfgMinGasPrice, mfd.Opts.MinGasPriceForHighGasTx)
	}
	if txfee_filters.IsArbTxLoose(tx) {
		cfgMinGasPrice = sdk.MaxDec(cfgMinGasPrice, mfd.Opts.MinGasPriceForArbitrageTx)
	}
	return cfgMinGasPrice
}

// DeductFeeDecorator deducts fees from the first signer of the tx.
// If the first signer does not have the funds to pay for the fees, we return an InsufficientFunds error.
// We call next AnteHandler if fees successfully deducted.
//
// CONTRACT: Tx must implement FeeTx interface to use DeductFeeDecorator
type DeductFeeDecorator struct {
	ak             types.AccountKeeper
	bankKeeper     types.BankKeeper
	feegrantKeeper types.FeegrantKeeper
	txFeesKeeper   Keeper
}

func NewDeductFeeDecorator(tk Keeper, ak types.AccountKeeper, bk types.BankKeeper, fk types.FeegrantKeeper) DeductFeeDecorator {
	return DeductFeeDecorator{
		ak:             ak,
		bankKeeper:     bk,
		feegrantKeeper: fk,
		txFeesKeeper:   tk,
	}
}

func (dfd DeductFeeDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (newCtx sdk.Context, err error) {
	feeTx, ok := tx.(sdk.FeeTx)
	if !ok {
		return ctx, sdkerrors.Wrap(sdkerrors.ErrTxDecode, "Tx must be a FeeTx")
	}

	// checks to make sure the module account has been set to collect fees in base token
	if addr := dfd.ak.GetModuleAddress(types.FeeCollectorName); addr == nil {
		return ctx, fmt.Errorf("Fee collector module account (%s) has not been set", types.FeeCollectorName)
	}

	// checks to make sure a separate module account has been set to collect fees not in base token
	if addrNonNativeFee := dfd.ak.GetModuleAddress(types.NonNativeFeeCollectorName); addrNonNativeFee == nil {
		return ctx, fmt.Errorf("non native fee collector module account (%s) has not been set", types.NonNativeFeeCollectorName)
	}

	// fee can be in any denom (checked for validity later)
	fee := feeTx.GetFee()
	feePayer := feeTx.FeePayer()
	feeGranter := feeTx.FeeGranter()

	// set the fee payer as the default address to deduct fees from
	deductFeesFrom := feePayer

	// If a fee granter was set, deduct fee from the fee granter's account.
	if feeGranter != nil {
		if dfd.feegrantKeeper == nil {
			return ctx, sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "fee grants is not enabled")
		} else if !feeGranter.Equals(feePayer) {
			err := dfd.feegrantKeeper.UseGrantedFees(ctx, feeGranter, feePayer, fee, tx.GetMsgs())
			if err != nil {
				return ctx, sdkerrors.Wrapf(err, "%s not allowed to pay fees from %s", feeGranter, feePayer)
			}
		}

		// if no errors, change the account that is charged for fees to the fee granter
		deductFeesFrom = feeGranter
	}

	deductFeesFromAcc := dfd.ak.GetAccount(ctx, deductFeesFrom)
	if deductFeesFromAcc == nil {
		return ctx, sdkerrors.Wrapf(sdkerrors.ErrUnknownAddress, "fee payer address: %s does not exist", deductFeesFrom)
	}

	// deducts the fees and transfer them to the module account
	if !feeTx.GetFee().IsZero() {
		err = DeductFees(dfd.txFeesKeeper, dfd.bankKeeper, ctx, deductFeesFromAcc, feeTx.GetFee())
		if err != nil {
			return ctx, err
		}
	}

	ctx.EventManager().EmitEvents(sdk.Events{sdk.NewEvent(sdk.EventTypeTx,
		sdk.NewAttribute(sdk.AttributeKeyFee, feeTx.GetFee().String()),
	)})

	return next(ctx, tx, simulate)
}

// DeductFees deducts fees from the given account and transfers them to the set module account.
func DeductFees(txFeesKeeper types.TxFeesKeeper, bankKeeper types.BankKeeper, ctx sdk.Context, acc authtypes.AccountI, fees sdk.Coins) error {
	// Checks the validity of the fee tokens (sorted, have positive amount, valid and unique denomination)
	if !fees.IsValid() {
		return sdkerrors.Wrapf(sdkerrors.ErrInsufficientFee, "invalid fee amount: %s", fees)
	}

	// pulls base denom from TxFeesKeeper (should be uOSMO)
	baseDenom, err := txFeesKeeper.GetBaseDenom(ctx)
	if err != nil {
		return err
	}

	// checks if input fee is uOSMO (assumes only one fee token exists in the fees array (as per the check in mempoolFeeDecorator))
	if fees[0].Denom == baseDenom {
		// sends to FeeCollectorName module account
		err := bankKeeper.SendCoinsFromAccountToModule(ctx, acc.GetAddress(), types.FeeCollectorName, fees)
		if err != nil {
			return sdkerrors.Wrapf(sdkerrors.ErrInsufficientFunds, err.Error())
		}
	} else {
		// sends to NonNativeFeeCollectorName module account
		err := bankKeeper.SendCoinsFromAccountToModule(ctx, acc.GetAddress(), types.NonNativeFeeCollectorName, fees)
		if err != nil {
			return sdkerrors.Wrapf(sdkerrors.ErrInsufficientFunds, err.Error())
		}
	}

	return nil
}
